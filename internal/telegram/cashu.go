package telegram

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/cashu"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
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

	// Bare amount: "/cashu 210 [memo]" — mint-and-share into the current chat
	// (group drop) or plain mint in a DM. This is what most users type first.
	if amount, err := GetAmount(args[1]); err == nil && amount >= 1 {
		memo := ""
		if len(args) > 2 {
			memo = strings.Join(args[2:], " ")
		}
		return bot.requestCashuMint(ctx, amount, memo, !m.Private(), m.Chat)
	}

	// Named subcommands are DM-only: receive/list would leak tokens or private
	// state into the group, and mint/send have the bare-amount form there.
	if !m.Private() {
		bot.trySendMessage(m.Chat, fmt.Sprintf("🥜 Use `/cashu <amount>` here to share ecash, or DM @%s for all other cashu commands.", bot.Telegram.Me.Username))
		return ctx, errors.Create(errors.InvalidSyntaxError)
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

var (
	cashuMintConfirmationMenu = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnConfirmCashuMint       = cashuMintConfirmationMenu.Data("✅ Mint", "confirm_cashu_mint")
	btnCancelCashuMint        = cashuMintConfirmationMenu.Data("🚫 Cancel", "cancel_cashu_mint")
	btnCashuTipAmount         = cashuMintConfirmationMenu.Data("", "cashu_tip_amount")
)

// CashuMintRequest is a pending /cashu mint awaiting user confirmation.
type CashuMintRequest struct {
	*storage.Base
	TelegramID int64  `json:"cashu_mint_req_telegram_id"`
	Amount     int64  `json:"cashu_mint_req_amount"`
	Memo       string `json:"cashu_mint_req_memo"`
	Public     bool   `json:"cashu_mint_req_public"` // post token into the chat instead of DM
}

func (bot *TipBot) makeCashuMintConfirmKeyboard(id string) *tb.ReplyMarkup {
	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	confirmBtn := menu.Data("✅ Mint", "confirm_cashu_mint", id)
	cancelBtn := menu.Data("🚫 Cancel", "cancel_cashu_mint", id)
	menu.Inline(menu.Row(confirmBtn, cancelBtn))
	return menu
}

// cashuMintHandler handles /cashu mint <amount> [memo] in DMs.
func (bot *TipBot) cashuMintHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()

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

	return bot.requestCashuMint(ctx, amount, memo, false, m.Chat)
}

// cashuTipHandler handles /cashutip [amount] [memo] in a group. Without an
// amount it asks for one in the requester's DM (ForceReply state flow, same as
// invoices) and then posts the confirmable share back into the group.
func (bot *TipBot) cashuTipHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	if !internal.Configuration.Cashu.Enabled {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuDisabledMessage"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	args := strings.Fields(m.Text)
	if len(args) > 1 {
		if amount, err := GetAmount(args[1]); err == nil && amount >= 1 {
			memo := ""
			if len(args) > 2 {
				memo = strings.Join(args[2:], " ")
			}
			return bot.requestCashuMint(ctx, amount, memo, !m.Private(), m.Chat)
		}
	}

	// No amount given: show an amount picker right in the chat. One tap is
	// both amount entry and confirmation; only the requester's taps count.
	// (Telegram has no requester-only visible group messages, so buttons beat
	// a public "enter amount" prompt or a DM detour.)
	NewMessage(m, WithDuration(0, bot))
	req := &CashuMintRequest{
		Base:       storage.New(storage.ID(fmt.Sprintf("cashu-mint-req:%d:%s", m.Sender.ID, RandStringRunes(8)))),
		TelegramID: m.Sender.ID,
		Public:     !m.Private(),
	}
	if err := req.Set(req, bot.Bunt); err != nil {
		return ctx, err
	}

	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	var row []tb.Btn
	for _, amt := range []int64{21, 100, 210, 500, 1000} {
		row = append(row, menu.Data(fmt.Sprintf("%d", amt), "cashu_tip_amount", req.ID, strconv.FormatInt(amt, 10)))
	}
	cancelRow := menu.Row(menu.Data("🚫 Cancel", "cancel_cashu_mint", req.ID))
	menu.Inline(menu.Row(row...), cancelRow)

	pickerMsg := bot.trySendMessage(m.Chat, fmt.Sprintf("🥜 %s, pick a tip amount (sat):", GetUserStrMd(m.Sender)), menu)
	if pickerMsg != nil {
		// Unanswered picker self-cancels; re-check state first, the message is
		// edited into the live share once an amount is picked.
		reqID := req.ID
		time.AfterFunc(5*time.Minute, func() {
			check := &CashuMintRequest{Base: storage.New(storage.ID(reqID))}
			fn, err := check.Get(check, bot.Bunt)
			if err != nil {
				return
			}
			if r := fn.(*CashuMintRequest); r.Active {
				_ = r.Inactivate(r, bot.Bunt)
				bot.tryDeleteMessage(pickerMsg)
			}
		})
	}
	return ctx, nil
}

// cashuTipAmountHandler runs when the requester taps an amount on the picker.
// The tap is the confirmation: the picker message becomes the Collect share.
func (bot *TipBot) cashuTipAmountHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	// c.Data = "<reqID>|<amount>"
	parts := strings.Split(c.Data, "|")
	if len(parts) != 2 {
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}
	amount, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || amount < 1 {
		return ctx, errors.Create(errors.InvalidAmountError)
	}

	req := &CashuMintRequest{Base: storage.New(storage.ID(parts[0]))}
	fn, err := req.Get(req, bot.Bunt)
	if err != nil {
		bot.tryEditMessage(c.Message, "🥜 This tip request expired. Send the command again.", &tb.ReplyMarkup{})
		return ctx, err
	}
	req = fn.(*CashuMintRequest)
	if req.TelegramID != user.Telegram.ID {
		ctx.Context = context.WithValue(ctx, "callback_response", "🥜 Only the requester can pick the amount.")
		return ctx, errors.Create(errors.UnknownError)
	}
	if !req.Active {
		ctx.Context = context.WithValue(ctx, "callback_response", "🥜 Already being processed.")
		return ctx, errors.Create(errors.NotActiveError)
	}

	// Balance and pending-cap checks, deferred to tap time.
	if balance, err := bot.GetUserBalanceCached(user); err == nil && balance < amount {
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "balanceTooLowMessage"))
		return ctx, errors.Create(errors.BalanceToLowError)
	}
	if pending, err := bot.countPendingCashuTokens(user.Telegram.ID); err == nil && pending >= maxPendingCashuTokens {
		ctx.Context = context.WithValue(ctx, "callback_response", fmt.Sprintf("🥜 You have %d pending cashu tokens (max %d).", pending, maxPendingCashuTokens))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}
	_ = req.Inactivate(req, bot.Bunt)

	return bot.postCashuShare(ctx, user, c.Message, amount, req.Memo)
}

// requestCashuMint validates a mint/share request and asks for confirmation.
// Money only moves after the user taps Confirm (confirmCashuMintHandler).
// public = post the resulting token into chat (group drop); chat is where the
// confirmation (and later the Collect message) is posted.
func (bot *TipBot) requestCashuMint(ctx intercept.Context, amount int64, memo string, public bool, chat *tb.Chat) (intercept.Context, error) {
	m := ctx.Message()
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
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

	// Store the pending request and ask for confirmation.
	req := &CashuMintRequest{
		Base:       storage.New(storage.ID(fmt.Sprintf("cashu-mint-req:%d:%s", m.Sender.ID, RandStringRunes(8)))),
		TelegramID: m.Sender.ID,
		Amount:     amount,
		Memo:       memo,
		Public:     public,
	}
	if err := req.Set(req, bot.Bunt); err != nil {
		log.Errorf("[cashu mint] could not persist mint request: %s", err.Error())
		return ctx, err
	}

	var confirmText string
	if public {
		confirmText = fmt.Sprintf("🥜 %s wants to share a *%d sat* ecash token in this chat.\nThe first user to hit Collect gets the sats. Confirm?", GetUserStrMd(m.Sender), amount)
	} else {
		confirmText = fmt.Sprintf("🥜 Mint a *%d sat* cashu token?\nThis pays %d sat from your wallet to the mint `%s`.", amount, amount, str.MarkdownEscape(bot.CashuClient.MintURL()))
	}
	if len(memo) > 0 {
		confirmText += fmt.Sprintf("\n_Memo: %s_", str.MarkdownEscape(memo))
	}
	// The confirmation lives in the chat the command came from; only the
	// requester can press its buttons.
	confirmMsg := bot.trySendMessage(chat, confirmText, bot.makeCashuMintConfirmKeyboard(req.ID))
	if public {
		// Keep group chats tidy: drop the command message right away, and
		// self-cancel an unanswered confirmation after 5 minutes. The timer must
		// re-check state: on Confirm this same message is edited into the live
		// Collect share and must NOT be deleted.
		NewMessage(m, WithDuration(0, bot))
		if confirmMsg != nil {
			reqID := req.ID
			time.AfterFunc(5*time.Minute, func() {
				check := &CashuMintRequest{Base: storage.New(storage.ID(reqID))}
				fn, err := check.Get(check, bot.Bunt)
				if err != nil {
					return
				}
				if r := fn.(*CashuMintRequest); r.Active {
					_ = r.Inactivate(r, bot.Bunt)
					bot.tryDeleteMessage(confirmMsg)
				}
			})
		}
	}
	return ctx, nil
}

// confirmCashuMintHandler runs when the user taps Confirm on a pending mint.
func (bot *TipBot) confirmCashuMintHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	req := &CashuMintRequest{Base: storage.New(storage.ID(c.Data))}
	fn, err := req.Get(req, bot.Bunt)
	if err != nil {
		log.Errorf("[cashu confirm] request %s not found: %s", c.Data, err.Error())
		bot.tryEditMessage(c.Message, "🥜 This mint request expired. Send the command again.", &tb.ReplyMarkup{})
		return ctx, err
	}
	req = fn.(*CashuMintRequest)

	// Only the requester can confirm, and only once. Others get a toast so the
	// button doesn't appear dead.
	if req.TelegramID != user.Telegram.ID {
		ctx.Context = context.WithValue(ctx, "callback_response", "🥜 Only the requester can confirm this.")
		return ctx, errors.Create(errors.UnknownError)
	}
	if !req.Active {
		ctx.Context = context.WithValue(ctx, "callback_response", "🥜 Already being processed.")
		return ctx, errors.Create(errors.NotActiveError)
	}
	_ = req.Inactivate(req, bot.Bunt)

	// Re-check the cap: requests could be confirmed out of order.
	if pending, err := bot.countPendingCashuTokens(user.Telegram.ID); err == nil && pending >= maxPendingCashuTokens {
		bot.tryEditMessage(c.Message, fmt.Sprintf("🥜 You have %d pending cashu tokens (max %d).", pending, maxPendingCashuTokens), &tb.ReplyMarkup{})
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	if req.Public {
		return bot.postCashuShare(ctx, user, c.Message, req.Amount, req.Memo)
	}

	return bot.executeCashuMint(ctx, user, c.Sender, c.Message, req.Amount, req.Memo)
}

// postCashuShare turns msg into a collectable share message. No minting yet —
// the sender's wallet is only charged when someone actually collects
// (acceptInlineCashuHandler), backed by the same InlineCashu record the
// inline flow uses.
func (bot *TipBot) postCashuShare(ctx intercept.Context, user *lnbits.User, msg *tb.Message, amount int64, memo string) (intercept.Context, error) {
	id := fmt.Sprintf("cashu:%s:%d", RandStringRunes(10), amount)
	shareMsg := fmt.Sprintf(Translate(ctx, "cashuSendMessage"), GetUserStrMd(user.Telegram), amount)
	if len(memo) > 0 {
		shareMsg += fmt.Sprintf("\n_Memo: %s_", str.MarkdownEscape(memo))
	}
	ic := &InlineCashu{
		Base:         storage.New(storage.ID(id)),
		Message:      shareMsg,
		Amount:       amount,
		From:         user,
		Memo:         memo,
		LanguageCode: "en",
	}
	if err := ic.Set(ic, bot.Bunt); err != nil {
		log.Errorf("[cashu share] could not persist share: %s", err.Error())
		return ctx, err
	}
	bot.tryEditMessage(msg, shareMsg, bot.makeCashuKeyboard(ctx, id))
	log.Infof("[cashu share] %s shared %d sat collectable in chat %d", GetUserStr(user.Telegram), amount, msg.Chat.ID)
	return ctx, nil
}

// cancelCashuMintHandler runs when the user taps Cancel on a pending mint.
func (bot *TipBot) cancelCashuMintHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)

	req := &CashuMintRequest{Base: storage.New(storage.ID(c.Data))}
	fn, err := req.Get(req, bot.Bunt)
	if err != nil {
		bot.tryEditMessage(c.Message, Translate(ctx, "cashuSendCancelledMessage"), &tb.ReplyMarkup{})
		return ctx, err
	}
	req = fn.(*CashuMintRequest)
	if req.TelegramID != user.Telegram.ID {
		ctx.Context = context.WithValue(ctx, "callback_response", "🥜 Only the requester can cancel this.")
		return ctx, errors.Create(errors.UnknownError)
	}
	_ = req.Inactivate(req, bot.Bunt)
	bot.tryEditMessage(c.Message, Translate(ctx, "cashuSendCancelledMessage"), &tb.ReplyMarkup{})
	NewMessage(c.Message, WithDuration(10*time.Minute, bot))
	return ctx, nil
}

// executeCashuMint performs the actual quote -> pay -> mint flow after the
// user confirmed a private mint. statusMsg is edited in place.
func (bot *TipBot) executeCashuMint(ctx intercept.Context, user *lnbits.User, sender *tb.User, statusMsg *tb.Message, amount int64, memo string) (intercept.Context, error) {
	m := &tb.Message{Sender: sender} // ownership of the durable record
	bot.tryEditMessage(statusMsg, fmt.Sprintf(Translate(ctx, "cashuMintMessage"), amount))

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

	// Step 3: Wait until the MINT sees the payment as settled. Our Pay
	// returning is not enough — minting too early fails with "quote not paid".
	if paid, err := bot.CashuClient.WaitQuotePaid(quote.Quote, 30*time.Second); !paid {
		log.Warnf("[cashu mint] quote %s not settled after 30s (err=%v)", quote.Quote, err)
		bot.tryEditMessage(statusMsg, "🥜 Payment sent, but the mint hasn't confirmed it yet. Your token is saved — run /cashu recover in a moment to finish it.")
		return ctx, fmt.Errorf("mint quote not settled in time")
	}

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

	// The token string goes as its OWN message: it can exceed Telegram's
	// 1024-char photo caption limit, and a standalone message is easier to copy.
	caption := fmt.Sprintf(Translate(ctx, "cashuMintSuccessMessage"), amount)

	// Step 5: Generate QR code
	qr, err := qrcode.Encode(tokenStr, qrcode.Medium, 512)
	if err != nil {
		log.Errorf("[cashu mint] QR code generation failed: %s", err.Error())
		// Still send the token string even if QR fails
		bot.tryEditMessage(statusMsg, caption)
		bot.trySendMessage(sender, "`"+tokenStr+"`")
		return ctx, nil
	}

	// Delete status message and send QR + token
	bot.tryDeleteMessage(statusMsg)
	bot.trySendMessage(sender, &tb.Photo{
		File:    tb.FromReader(bytes.NewReader(qr)),
		Caption: caption,
	})
	bot.trySendMessage(sender, "`"+tokenStr+"`")

	log.Infof("[cashu mint] %s minted %d sat cashu token", GetUserStr(sender), amount)
	return ctx, nil
}

// cashuClaimAliasHandler handles top-level /claim <token> and /redeem <token>
// (observed users trying these naturally). Same as /cashu receive.
func (bot *TipBot) cashuClaimAliasHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	if !internal.Configuration.Cashu.Enabled {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuDisabledMessage"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}
	user := LoadUser(ctx)
	if user.Wallet.ID == "" {
		return ctx, errors.Create(errors.UserNoWalletError)
	}
	args := strings.Fields(m.Text)
	if len(args) < 2 {
		bot.trySendMessage(m.Sender, Translate(ctx, "cashuHelpText"))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}
	return bot.redeemCashuToken(ctx, args[1], user, m.Sender)
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

	// Step 2: Validate the token's mint URL matches our configured mint.
	// Normalize: trailing slashes and scheme/host case must not cause a
	// same-mint token to be rejected.
	tokenMintURL := token.Token[0].Mint
	if !sameMintURL(tokenMintURL, bot.CashuClient.MintURL()) {
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
	if resp.State == "PENDING" || resp.State == "" {
		// The mint's outbound Lightning payment is in flight — poll until it
		// settles instead of treating PENDING as failure.
		paid, werr := bot.CashuClient.WaitMeltPaid(mq.Quote, 45*time.Second)
		if !paid {
			return 0, fmt.Errorf("melt not settled (state=%s, err=%v)", resp.State, werr)
		}
	} else if resp.State != "PAID" {
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

	// Summary first, then each unclaimed token as its OWN message: several
	// full token strings in one message blow Telegram's 4096-char limit and
	// the send fails silently, making /cashu list appear dead.
	var sb strings.Builder
	sb.WriteString("🥜 *Your cashu tokens*\n")
	var unclaimed []*CashuToken
	shown := 0
	for _, c := range tokens {
		bot.refreshCashuTokenState(c) // may flip unclaimed -> spent
		switch c.State {
		case cashuStateMinting:
			sb.WriteString(fmt.Sprintf("\n⏳ *%d sat* — paid but not minted. Run /cashu recover.", c.Amount))
			shown++
		case cashuStateUnclaimed:
			sb.WriteString(fmt.Sprintf("\n🥜 *%d sat* — unclaimed (token below)", c.Amount))
			unclaimed = append(unclaimed, c)
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
	for _, c := range unclaimed {
		// QR first (scannable by any cashu wallet), token text after (copyable).
		// QR encoding fails for very large tokens (capacity ~3KB) — text still goes out.
		if qr, err := qrcode.Encode(c.Token, qrcode.Medium, 512); err == nil {
			bot.trySendMessage(m.Sender, &tb.Photo{
				File:    tb.FromReader(bytes.NewReader(qr)),
				Caption: fmt.Sprintf("🥜 %d sat", c.Amount),
			})
		}
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 *%d sat*:\n`%s`", c.Amount, c.Token))
	}
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

	recovered, stillPending := 0, 0
	for _, c := range tokens {
		if c.State != cashuStateMinting {
			continue
		}
		q, err := bot.CashuClient.CheckMintQuote(c.QuoteId)
		if err != nil {
			// Never report a pending record as "nothing": tell the user it exists.
			log.Errorf("[cashu recover] check quote %s: %s", c.QuoteId, err.Error())
			stillPending++
			continue
		}
		if q.State == "ISSUED" {
			// Tokens were already issued for this quote but never stored (lost mint
			// response). They can't be re-derived; stop showing it as pending.
			c.State = cashuStateSpent
			_ = bot.setCashuToken(c)
			continue
		}
		if !q.IsPaid() {
			// Legacy mints answer with paid=false while settling; newer ones with
			// state=UNPAID. Either way: not claimable yet, but still the user's.
			log.Warnf("[cashu recover] quote %s not paid yet (state=%q paid=%v)", c.QuoteId, q.State, q.Paid)
			stillPending++
			continue
		}
		tokenStr, err := bot.CashuClient.MintTokens(c.QuoteId, c.Amount, c.Memo)
		if err != nil {
			log.Errorf("[cashu recover] mint from quote %s failed: %s", c.QuoteId, err.Error())
			stillPending++
			continue
		}
		c.Token = tokenStr
		c.State = cashuStateUnclaimed
		_ = bot.setCashuToken(c)
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 Recovered *%d sat*:\n`%s`", c.Amount, tokenStr))
		recovered++
	}

	switch {
	case recovered > 0 && stillPending == 0:
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 Recovered %d token(s) to your wallet.", recovered))
	case stillPending > 0:
		bot.trySendMessage(m.Sender, fmt.Sprintf("🥜 Recovered %d token(s). %d still pending — the mint hasn't settled or answered yet, try again in a bit. Your sats are not lost.", recovered, stillPending))
	default:
		bot.trySendMessage(m.Sender, "🥜 Nothing to recover.")
	}
	return ctx, nil
}

// sameMintURL compares two mint URLs ignoring trailing slashes and case.
// ponytail: string-level compare is enough for one configured mint; full URL
// parsing only if multi-mint support ever lands.
func sameMintURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

// truncate shortens a string for logging.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
