package telegram

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/cashu"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/skip2/go-qrcode"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// cashuHandler is the main /cashu command router.
func (bot *TipBot) cashuHandler(ctx intercept.Context) (intercept.Context, error) {
	if !internal.Configuration.Cashu.Enabled {
		bot.trySendMessage(ctx.Message().Sender, Translate(ctx, "cashuDisabledMessage"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	m := ctx.Message()
	if m.Text == "" {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, nil
	}

	args := strings.Fields(m.Text)
	if len(args) < 2 {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, nil
	}

	subcommand := strings.ToLower(args[1])
	switch subcommand {
	case "mint":
		return bot.cashuMintHandler(ctx)
	case "receive", "redeem", "claim":
		return bot.cashuReceiveHandler(ctx)
	case "send":
		return bot.cashuSendHandler(ctx)
	default:
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, nil
	}
}

// cashuMintHandler handles /cashu mint <amount> [memo]
// Creates ecash tokens by paying the external mint's Lightning invoice.
func (bot *TipBot) cashuMintHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	// Parse: /cashu mint <amount> [memo]
	args := strings.Fields(m.Text)
	if len(args) < 3 {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	amount, err := GetAmount(args[2])
	if err != nil || amount < 1 {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, errors.New(errors.InvalidAmountError, fmt.Errorf("invalid cashu mint amount"))
	}

	memo := ""
	if len(args) > 3 {
		memo = strings.Join(args[3:], " ")
	}

	// Check user balance
	balance, err := bot.GetUserBalanceCached(user)
	if err != nil {
		log.Errorf("[cashu mint] Error getting balance: %s", err.Error())
		return ctx, errors.New(errors.GetBalanceError, err)
	}
	if balance < amount {
		bot.trySendMessage(m.Sender, Translate(ctx, "balanceTooLowMessage"))
		return ctx, errors.New(errors.BalanceToLowError, fmt.Errorf("balance too low for cashu mint"))
	}

	// Notify user we're working on it
	statusMsg := bot.trySendMessage(m.Sender, fmt.Sprintf(Translate(ctx, "cashuMintMessage"), amount))

	// Step 1: Request mint quote from the external Cashu mint
	quote, err := bot.CashuClient.MintQuote(amount, "sat")
	if err != nil {
		log.Errorf("[cashu mint] MintQuote error: %s", err.Error())
		bot.tryEditMessage(statusMsg, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	log.Infof("[cashu mint] Got quote %s for %d sat, invoice: %s...", quote.Quote, amount, truncate(quote.Request, 30))

	// Step 2: Pay the mint's Lightning invoice from user's wallet
	_, err = user.Wallet.Pay(lnbits.PaymentParams{
		Out:    true,
		Bolt11: quote.Request,
	}, bot.Client)
	if err != nil {
		log.Errorf("[cashu mint] Payment to mint failed: %s", err.Error())
		bot.tryEditMessage(statusMsg, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	log.Infof("[cashu mint] Paid mint invoice for quote %s", quote.Quote)

	// Step 3: Wait briefly for payment to settle, then check quote status
	time.Sleep(2 * time.Second)

	// Step 4: Mint the tokens
	tokenStr, err := bot.CashuClient.MintTokens(quote.Quote, amount, memo)
	if err != nil {
		log.Errorf("[cashu mint] MintTokens failed: %s", err.Error())
		bot.tryEditMessage(statusMsg, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	// Step 5: Generate QR code
	qr, err := qrcode.Encode(tokenStr, qrcode.Medium, 512)
	if err != nil {
		log.Errorf("[cashu mint] QR code generation failed: %s", err.Error())
		// Still send the token string even if QR fails
		bot.tryEditMessage(statusMsg, fmt.Sprintf(Translate(ctx, "cashuMintSuccessMessage"), amount)+"\n\n`"+tokenStr+"`")
		return ctx, nil
	}

	// Delete status message and send QR + token
	bot.tryDeleteMessage(statusMsg)

	caption := fmt.Sprintf(Translate(ctx, "cashuMintSuccessMessage"), amount) + "\n\n`" + tokenStr + "`"
	photo := &tb.Photo{
		File:    tb.FromReader(bytes.NewReader(qr)),
		Caption: caption,
	}
	bot.trySendMessage(m.Sender, photo)

	log.Infof("[cashu mint] %s minted %d sat cashu token", GetUserStr(m.Sender), amount)
	return ctx, nil
}

// cashuReceiveHandler handles /cashu receive <token>
// Redeems a cashu token by melting it at the mint (mint pays a Lightning invoice to user's wallet).
func (bot *TipBot) cashuReceiveHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	// Parse: /cashu receive <token>
	args := strings.Fields(m.Text)
	if len(args) < 3 {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	tokenStr := args[2]

	return bot.redeemCashuToken(ctx, tokenStr, user, m.Sender)
}

// cashuSendHandler handles /cashu send <amount> [memo]
// Creates a cashu token (same as mint) for the purpose of sharing.
func (bot *TipBot) cashuSendHandler(ctx intercept.Context) (intercept.Context, error) {
	// Send is functionally the same as mint - it creates a token the user can share
	return bot.cashuMintHandler(ctx)
}

// redeemCashuToken is the shared logic for receiving/redeeming a cashu token.
func (bot *TipBot) redeemCashuToken(ctx intercept.Context, tokenStr string, user *lnbits.User, recipient *tb.User) (intercept.Context, error) {
	// Step 1: Deserialize the token
	token, err := cashu.Deserialize(tokenStr)
	if err != nil {
		log.Warnf("[cashu receive] Invalid token: %s", err.Error())
		bot.trySendMessage(recipient, Translate(ctx, "cashuTokenInvalidMessage"))
		return ctx, err
	}

	if len(token.Token) == 0 || len(token.Token[0].Proofs) == 0 {
		bot.trySendMessage(recipient, Translate(ctx, "cashuTokenInvalidMessage"))
		return ctx, fmt.Errorf("token has no proofs")
	}

	// Step 2: Validate the token's mint URL matches our configured mint
	tokenMintURL := token.Token[0].Mint
	if tokenMintURL != bot.CashuClient.MintURL() {
		log.Warnf("[cashu receive] Token from unknown mint: %s (expected: %s)", tokenMintURL, bot.CashuClient.MintURL())
		bot.trySendMessage(recipient, fmt.Sprintf(Translate(ctx, "cashuMintMismatchMessage"), bot.CashuClient.MintURL()))
		return ctx, fmt.Errorf("mint mismatch")
	}

	proofs := token.Token[0].Proofs
	totalAmount := token.Amount()

	// Step 3: Check if proofs are unspent (NUT-07)
	unspent, err := bot.CashuClient.AllProofsUnspent(proofs)
	if err != nil {
		log.Warnf("[cashu receive] State check failed: %s", err.Error())
		// Don't abort - some mints may not support NUT-07. Proceed and let melt fail if spent.
	} else if !unspent {
		bot.trySendMessage(recipient, Translate(ctx, "cashuTokenSpentMessage"))
		return ctx, fmt.Errorf("token already spent")
	}

	// Step 4: Create a Lightning invoice on the user's LNbits wallet
	invoice, err := user.Wallet.Invoice(lnbits.InvoiceParams{
		Out:    false,
		Amount: totalAmount,
		Memo:   fmt.Sprintf("Cashu ecash redeem (%d sat)", totalAmount),
	}, bot.Client)
	if err != nil {
		log.Errorf("[cashu receive] Failed to create invoice: %s", err.Error())
		bot.trySendMessage(recipient, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	// Step 5: Request melt quote from mint (ask mint to pay our invoice)
	meltQuote, err := bot.CashuClient.MeltQuote(invoice.PaymentRequest, "sat")
	if err != nil {
		log.Errorf("[cashu receive] MeltQuote failed: %s", err.Error())
		bot.trySendMessage(recipient, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	// Check if the melt will consume more than the token provides (fees)
	meltCost := meltQuote.Amount + meltQuote.FeeReserve
	if meltCost > totalAmount {
		log.Warnf("[cashu receive] Melt cost (%d) exceeds token amount (%d)", meltCost, totalAmount)
		bot.trySendMessage(recipient, fmt.Sprintf(Translate(ctx, "cashuFeeTooHighMessage"), totalAmount, meltCost))
		return ctx, fmt.Errorf("melt fees too high")
	}

	// Step 6: Melt the proofs (mint pays the invoice)
	meltResp, err := bot.CashuClient.Melt(meltQuote.Quote, proofs)
	if err != nil {
		log.Errorf("[cashu receive] Melt failed: %s", err.Error())
		bot.trySendMessage(recipient, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	if meltResp.State != "PAID" {
		log.Warnf("[cashu receive] Melt not paid, state: %s", meltResp.State)
		bot.trySendMessage(recipient, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, fmt.Errorf("melt state: %s", meltResp.State)
	}

	// Success!
	bot.trySendMessage(recipient, fmt.Sprintf(Translate(ctx, "cashuReceiveSuccessMessage"), totalAmount))
	log.Infof("[cashu receive] %s redeemed %d sat cashu token", GetUserStr(recipient), totalAmount)

	return ctx, nil
}

// truncate shortens a string for logging.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
