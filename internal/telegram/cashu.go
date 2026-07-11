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
	case "list", "pending", "tokens":
		return bot.cashuListHandler(ctx)
	case "recover":
		return bot.cashuRecoverHandler(ctx)
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

	// DoS guard: cap pending (minting/unclaimed) tokens per user.
	if pending, err := bot.countPendingCashuTokens(m.Sender.ID); err == nil && pending >= maxPendingCashuTokens {
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 You have %d pending cashu tokens (max %d). Redeem some with /cashu list or finish them with /cashu recover first.", pending, maxPendingCashuTokens))
		return ctx, errors.Create(errors.InvalidSyntaxError)
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

	// Persist a durable record BEFORE paying so a paid-but-not-minted quote is
	// never silently lost — /cashu recover can finish it. Starts as "minting".
	record := newCashuToken(m.Sender.ID, m.Sender.Username, amount, memo, quote.Quote)
	if err := bot.setCashuToken(record); err != nil {
		log.Errorf("[cashu mint] could not persist token record: %s", err.Error())
		bot.tryEditMessage(statusMsg, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	// Step 2: Pay the mint's Lightning invoice from user's wallet
	_, err = user.Wallet.Pay(lnbits.PaymentParams{
		Out:    true,
		Bolt11: quote.Request,
	}, bot.Client)
	if err != nil {
		// Nothing was paid — drop the record so it doesn't show as recoverable.
		log.Errorf("[cashu mint] Payment to mint failed: %s", err.Error())
		_ = record.Delete(record, bot.Bunt)
		bot.tryEditMessage(statusMsg, Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}

	log.Infof("[cashu mint] Paid mint invoice for quote %s", quote.Quote)

	// Step 3: Wait briefly for payment to settle, then check quote status
	time.Sleep(2 * time.Second)

	// Step 4: Mint the tokens
	tokenStr, err := bot.CashuClient.MintTokens(quote.Quote, amount, memo)
	if err != nil {
		// PAID but not minted. Keep the "minting" record so /cashu recover can
		// finish it. ponytail: recovery re-mints from the paid quote; only a lost
		// mint response (quote already ISSUED) is unrecoverable, which is rare.
		log.Errorf("[cashu mint] MintTokens failed after pay, recoverable quote=%s: %s", quote.Quote, err.Error())
		bot.tryEditMessage(statusMsg, "🥜 Your payment went through, but the mint hasn't returned the token yet. It's saved — run /cashu recover to finish it.")
		return ctx, err
	}

	// Token minted: mark the record unclaimed and store the token string.
	record.Token = tokenStr
	record.State = cashuStateUnclaimed
	_ = bot.setCashuToken(record)

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

	// Step 4-6: melt the proofs so the mint pays an invoice on the user's wallet.
	netAmount, err := bot.meltProofsToWallet(user, proofs, totalAmount)
	if err != nil {
		log.Errorf("[cashu receive] melt failed: %s", err.Error())
		if strings.Contains(err.Error(), "exceed") {
			bot.trySendMessage(recipient, fmt.Sprintf(Translate(ctx, "cashuFeeTooHighMessage"), totalAmount, totalAmount))
		} else {
			bot.trySendMessage(recipient, Translate(ctx, "cashuMintErrorMessage"))
		}
		return ctx, err
	}

	// If this was one of the user's own stored tokens, mark it spent.
	bot.markCashuTokenSpent(user.Telegram.ID, tokenStr)

	bot.trySendMessage(recipient, fmt.Sprintf(Translate(ctx, "cashuReceiveSuccessMessage"), netAmount))
	log.Infof("[cashu receive] %s redeemed %d sat cashu token (net %d)", GetUserStr(recipient), totalAmount, netAmount)

	return ctx, nil
}

// meltProofsToWallet melts proofs at the mint so the mint pays a Lightning
// invoice on the given wallet, covering the mint's melt fee by invoicing for
// (totalAmount - fee). Returns the net sats credited. Shared by /cashu receive
// and the inline claim so the fee math lives in exactly one place.
func (bot *TipBot) meltProofsToWallet(user *lnbits.User, proofs []cashu.Proof, totalAmount int64) (int64, error) {
	// quoteFor creates an invoice for amt on the user's wallet and asks the mint
	// how much melting to it would cost.
	quoteFor := func(amt int64) (*cashu.MeltQuoteResponse, error) {
		inv, err := user.Wallet.Invoice(lnbits.InvoiceParams{
			Out:    false,
			Amount: amt,
			Memo:   fmt.Sprintf("Cashu redeem (%d sat)", amt),
		}, bot.Client)
		if err != nil {
			return nil, fmt.Errorf("invoice: %w", err)
		}
		return bot.CashuClient.MeltQuote(inv.PaymentRequest, "sat")
	}

	mq, err := quoteFor(totalAmount)
	if err != nil {
		return 0, err
	}
	net := totalAmount
	if mq.FeeReserve > 0 {
		// Proofs only cover totalAmount, so the mint must pay less than that to
		// leave room for its fee. ponytail: the first (full-amount) invoice is
		// left unpaid and simply expires — cheaper than a separate fee-estimate.
		net = totalAmount - mq.FeeReserve
		if net <= 0 {
			return 0, fmt.Errorf("melt fees (%d) exceed token amount (%d)", mq.FeeReserve, totalAmount)
		}
		mq, err = quoteFor(net)
		if err != nil {
			return 0, err
		}
		if mq.Amount+mq.FeeReserve > totalAmount {
			return 0, fmt.Errorf("melt cost (%d) exceeds token amount (%d)", mq.Amount+mq.FeeReserve, totalAmount)
		}
	}

	resp, err := bot.CashuClient.Melt(mq.Quote, proofs)
	if err != nil {
		return 0, err
	}
	if resp.State != "PAID" {
		return 0, fmt.Errorf("melt state: %s", resp.State)
	}
	return net, nil
}

// cashuListHandler handles /cashu list — shows the user's cashu tokens,
// greying out / hiding claimed ones.
func (bot *TipBot) cashuListHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)
	tokens, err := bot.listCashuTokens(user.Telegram.ID)
	if err != nil {
		log.Errorf("[cashu list] %s", err.Error())
		return ctx, err
	}

	var sb strings.Builder
	sb.WriteString("🥜 *Your cashu tokens*\n")
	shown := 0
	for _, c := range tokens {
		bot.refreshCashuTokenState(c) // may flip unclaimed -> spent
		switch c.State {
		case cashuStateMinting:
			sb.WriteString(fmt.Sprintf("\n⏳ *%d sat* — paid but not minted. Run /cashu recover.", c.Amount))
			shown++
		case cashuStateUnclaimed:
			sb.WriteString(fmt.Sprintf("\n🥜 *%d sat* unclaimed:\n`%s`\n", c.Amount, c.Token))
			shown++
		case cashuStateSpent:
			// Greyed out, and hidden entirely once it's been claimed a while.
			if time.Since(c.CreatedAt) < 24*time.Hour {
				sb.WriteString(fmt.Sprintf("\n~%d sat — claimed~", c.Amount))
				shown++
			}
		}
	}

	if shown == 0 {
		bot.trySendMessage(m.Sender, "🥜 You have no active cashu tokens.")
		return ctx, nil
	}
	bot.trySendMessage(m.Sender, sb.String())
	return ctx, nil
}

// cashuRecoverHandler handles /cashu recover — finishes any paid-but-not-minted
// quotes so the user's sats are never left stranded.
func (bot *TipBot) cashuRecoverHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)
	tokens, err := bot.listCashuTokens(user.Telegram.ID)
	if err != nil {
		return ctx, err
	}

	recovered := 0
	for _, c := range tokens {
		if c.State != cashuStateMinting {
			continue
		}
		q, err := bot.CashuClient.CheckMintQuote(c.QuoteId)
		if err != nil {
			log.Errorf("[cashu recover] check quote %s: %s", c.QuoteId, err.Error())
			continue
		}
		if q.State == "ISSUED" {
			// Tokens were already issued for this quote but never stored (lost mint
			// response). They can't be re-derived; stop showing it as pending.
			c.State = cashuStateSpent
			_ = bot.setCashuToken(c)
			continue
		}
		if q.State != "PAID" {
			continue
		}
		tokenStr, err := bot.CashuClient.MintTokens(c.QuoteId, c.Amount, c.Memo)
		if err != nil {
			log.Errorf("[cashu recover] mint from quote %s failed: %s", c.QuoteId, err.Error())
			continue
		}
		c.Token = tokenStr
		c.State = cashuStateUnclaimed
		_ = bot.setCashuToken(c)
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 Recovered *%d sat*:\n`%s`", c.Amount, tokenStr))
		recovered++
	}

	if recovered == 0 {
		bot.trySendMessage(m.Sender, "🥜 Nothing to recover.")
	} else {
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 Recovered %d token(s) to your wallet.", recovered))
	}
	return ctx, nil
}

// truncate shortens a string for logging.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
