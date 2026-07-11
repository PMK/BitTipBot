package telegram

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/cashu"
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	"github.com/LightningTipBot/LightningTipBot/internal/str"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"

	"github.com/eko/gocache/store"

	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

var (
	inlineCashuMenu      = &tb.ReplyMarkup{ResizeKeyboard: true}
	btnClaimInlineCashu  = inlineCashuMenu.Data("🥜 Claim", "claim_cashu_inline")
	btnCancelInlineCashu = inlineCashuMenu.Data("🚫 Cancel", "cancel_cashu_inline")
)

// InlineCashu represents a cashu token shared in a group chat via inline query.
type InlineCashu struct {
	*storage.Base
	Message      string       `json:"inline_cashu_message"`
	Amount       int64        `json:"inline_cashu_amount"`
	From         *lnbits.User `json:"inline_cashu_from"`
	Token        string       `json:"inline_cashu_token"`
	Memo         string       `json:"inline_cashu_memo"`
	Claimed      bool         `json:"inline_cashu_claimed"`
	ClaimedBy    *lnbits.User `json:"inline_cashu_claimed_by,omitempty"`
	LanguageCode string       `json:"languagecode"`
}

func (bot *TipBot) makeCashuKeyboard(ctx context.Context, id string) *tb.ReplyMarkup {
	menu := &tb.ReplyMarkup{ResizeKeyboard: true}
	claimBtn := menu.Data(Translate(ctx, "cashuClaimButtonMessage"), "claim_cashu_inline", id)
	cancelBtn := menu.Data(Translate(ctx, "cancelButtonMessage"), "cancel_cashu_inline", id)
	menu.Inline(
		menu.Row(claimBtn, cancelBtn),
	)
	return menu
}

// handleInlineCashuQuery handles inline query: @bot cashu <amount> [memo]
func (bot *TipBot) handleInlineCashuQuery(ctx intercept.Context) (intercept.Context, error) {
	if !internal.Configuration.Cashu.Enabled {
		bot.inlineQueryReplyWithError(ctx, "Cashu disabled", "Cashu ecash support is not enabled on this bot.")
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	query := ctx.Query()
	text := query.Text
	args := strings.Fields(text)

	// cashu <amount> [memo]
	if len(args) < 2 {
		bot.inlineQueryReplyWithError(ctx,
			TranslateUser(ctx, "inlineQueryCashuTitle"),
			fmt.Sprintf(TranslateUser(ctx, "inlineQueryCashuDescription"), bot.Telegram.Me.Username))
		return ctx, nil
	}

	amount, err := GetAmount(args[1])
	if err != nil || amount < 1 {
		bot.inlineQueryReplyWithError(ctx, "Invalid amount", fmt.Sprintf(TranslateUser(ctx, "inlineQueryCashuDescription"), bot.Telegram.Me.Username))
		return ctx, nil
	}

	memo := ""
	if len(args) > 2 {
		memo = strings.Join(args[2:], " ")
	}

	fromUser := LoadUser(ctx)
	fromUserStr := GetUserStr(query.Sender)

	// Check balance
	balance, err := bot.GetUserBalanceCached(fromUser)
	if err != nil {
		bot.inlineQueryReplyWithError(ctx, "Error", "Could not check balance.")
		return ctx, err
	}
	if balance < amount {
		bot.inlineQueryReplyWithError(ctx,
			TranslateUser(ctx, "balanceTooLowMessage"),
			fmt.Sprintf(TranslateUser(ctx, "inlineQueryCashuDescription"), bot.Telegram.Me.Username))
		return ctx, nil
	}

	// DoS guard: same pending cap as /cashu mint, so a capped user can't keep
	// creating claimable offers (each failed delivery would bank another token).
	if pending, err := bot.countPendingCashuTokens(query.Sender.ID); err == nil && pending >= maxPendingCashuTokens {
		bot.inlineQueryReplyWithError(ctx, "Too many pending cashu tokens", fmt.Sprintf("You have %d pending tokens (max %d). Redeem or recover them first.", pending, maxPendingCashuTokens))
		return ctx, errors.Create(errors.InvalidSyntaxError)
	}

	// ponytail: do NOT mint here — inline queries fire on every keystroke, so
	// minting here spends money per character typed. The token is minted from
	// the sender's wallet on claim instead (acceptInlineCashuHandler).

	// Create the inline cashu object
	id := fmt.Sprintf("cashu:%s:%d", RandStringRunes(10), amount)
	inlineMessage := fmt.Sprintf(Translate(ctx, "cashuSendMessage"), GetUserStrMd(query.Sender), amount)
	if len(memo) > 0 {
		// User-controlled text rendered as Markdown in a public chat — escape it.
		inlineMessage += fmt.Sprintf("\n_Memo: %s_", str.MarkdownEscape(memo))
	}

	inlineCashu := &InlineCashu{
		Base:         storage.New(storage.ID(id)),
		Message:      inlineMessage,
		Amount:       amount,
		From:         fromUser,
		Token:        "", // minted on claim
		Memo:         memo,
		Claimed:      false,
		LanguageCode: "en",
	}

	// Build inline result
	results := make(tb.Results, 1)
	result := &tb.ArticleResult{
		Text:        inlineMessage,
		Title:       fmt.Sprintf(TranslateUser(ctx, "inlineResultCashuTitle"), amount),
		Description: TranslateUser(ctx, "inlineResultCashuDescription"),
		ThumbURL:    queryImage,
	}
	result.SetParseMode(tb.ModeMarkdown)
	result.ReplyMarkup = &tb.ReplyMarkup{InlineKeyboard: bot.makeCashuKeyboard(ctx, inlineCashu.ID).InlineKeyboard}
	results[0] = result
	results[0].SetResultID(inlineCashu.ID)

	// Cache for later retrieval
	bot.Cache.Set(inlineCashu.ID, inlineCashu, &store.Options{Expiration: 5 * time.Minute})

	log.Infof("[cashu inline] %s created inline cashu %s: %d sat", fromUserStr, id, amount)

	err = bot.Telegram.Answer(ctx.Query(), &tb.QueryResponse{
		Results:   results,
		CacheTime: 1,
	})
	if err != nil {
		log.Errorln(err.Error())
	}
	return ctx, nil
}

// acceptInlineCashuHandler handles the "Claim" button click on an inline cashu token.
func (bot *TipBot) acceptInlineCashuHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	to := LoadUser(ctx)

	tx := &InlineCashu{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	fn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[acceptInlineCashuHandler] c.Data: %s, Error: %s", c.Data, err.Error())
		return ctx, err
	}

	inlineCashu := fn.(*InlineCashu)
	from := inlineCashu.From

	// Check if already claimed
	if !inlineCashu.Active || inlineCashu.Claimed {
		log.Tracef("[cashu] token %s already claimed", inlineCashu.ID)
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "cashuAlreadyClaimedMessage"))
		return ctx, errors.Create(errors.NotActiveError)
	}

	// Can't claim your own token
	if from.Telegram.ID == to.Telegram.ID {
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "sendYourselfMessage"))
		return ctx, errors.Create(errors.SelfPaymentError)
	}

	// Create wallet for recipient if needed
	if !to.Initialized {
		_, err = bot.CreateWalletForTelegramUser(to.Telegram)
		if err != nil {
			log.Errorf("[cashu claim] Failed to create wallet: %s", err.Error())
			return ctx, err
		}
		to = LoadUser(ctx) // reload
	}

	// Mint the token now, from the sender's wallet. Deferred to claim so inline
	// query keystrokes never spend money — only an actual claim does.
	amount := inlineCashu.Amount
	quote, err := bot.CashuClient.MintQuote(amount, "sat")
	if err != nil {
		log.Errorf("[cashu claim] MintQuote failed: %s", err.Error())
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}
	// Durable record BEFORE paying, exactly like /cashu mint: if anything after
	// the payment fails, the sender recovers via /cashu recover instead of
	// losing the sats.
	record := newCashuToken(from.Telegram.ID, from.Telegram.Username, amount, inlineCashu.Memo, quote.Quote)
	if err := bot.setCashuToken(record); err != nil {
		log.Errorf("[cashu claim] could not persist token record: %s", err.Error())
		return ctx, err
	}
	if _, err = from.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: quote.Request}, bot.Client); err != nil {
		log.Errorf("[cashu claim] sender %s could not pay mint invoice: %s", GetUserStr(from.Telegram), err.Error())
		_ = record.Delete(record, bot.Bunt) // nothing was paid
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "balanceTooLowMessage"))
		return ctx, err
	}
	if paid, werr := bot.CashuClient.WaitQuotePaid(quote.Quote, 30*time.Second); !paid {
		log.Errorf("[cashu claim] quote %s not settled after 30s (err=%v), sender can /cashu recover", quote.Quote, werr)
		bot.trySendMessage(from.Telegram, "🥜 Your inline cashu payment hasn't settled at the mint yet. Run /cashu recover in a moment to reclaim it.")
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "cashuMintErrorMessage"))
		return ctx, fmt.Errorf("mint quote not settled in time")
	}
	tokenStr, err := bot.CashuClient.MintTokens(quote.Quote, amount, inlineCashu.Memo)
	if err != nil {
		// Sender paid; the minting-state record makes this recoverable.
		log.Errorf("[cashu claim] MintTokens failed AFTER sender paid, recoverable quote=%s: %s", quote.Quote, err.Error())
		bot.trySendMessage(from.Telegram, "🥜 Your inline cashu couldn't be minted yet. Run /cashu recover to reclaim it.")
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "cashuMintErrorMessage"))
		return ctx, err
	}
	record.Token = tokenStr
	record.State = cashuStateUnclaimed
	_ = bot.setCashuToken(record)

	token, err := cashu.Deserialize(tokenStr)
	if err != nil {
		log.Errorf("[cashu claim] deserialize freshly minted token failed: %s", err.Error())
		return ctx, err
	}

	proofs := token.Token[0].Proofs
	totalAmount := token.Amount()

	// Deliver to the claimer by melting the proofs onto their wallet.
	netAmount, err := bot.meltProofsToWallet(to, proofs, totalAmount)
	if err != nil {
		// Sender was debited and a token minted, but delivery failed. The
		// unclaimed record (stored above) keeps the token in the sender's wallet.
		log.Errorf("[cashu claim] melt to claimer failed, token stays with sender %s: %s", GetUserStr(from.Telegram), err.Error())
		bot.trySendMessage(from.Telegram, "🥜 Your inline cashu couldn't be delivered, so the token was saved to your wallet — see /cashu list.")
		return ctx, err
	}
	totalAmount = netAmount

	// Token consumed by the claimer: mark the sender's record spent.
	record.State = cashuStateSpent
	_ = bot.setCashuToken(record)

	// Mark as claimed
	inlineCashu.Claimed = true
	inlineCashu.ClaimedBy = to
	inlineCashu.Active = false
	err = inlineCashu.Set(inlineCashu, bot.Bunt)
	if err != nil {
		log.Errorf("[cashu claim] Failed to update bunt: %s", err.Error())
	}

	// Update inline message
	claimedMessage := fmt.Sprintf(Translate(ctx, "cashuSendClaimedMessage"), totalAmount, GetUserStrMd(to.Telegram))
	bot.tryEditMessage(c.Message, claimedMessage, &tb.ReplyMarkup{})

	// Notify the sender
	bot.trySendMessage(from.Telegram, fmt.Sprintf("🥜 Your %d sat cashu token was claimed by %s.", totalAmount, GetUserStr(to.Telegram)))

	log.Infof("[cashu claim] %s claimed %d sat cashu token from %s", GetUserStr(to.Telegram), totalAmount, GetUserStr(from.Telegram))
	return ctx, nil
}

// cancelInlineCashuHandler handles the "Cancel" button click (only the creator can cancel).
func (bot *TipBot) cancelInlineCashuHandler(ctx intercept.Context) (intercept.Context, error) {
	c := ctx.Callback()
	user := LoadUser(ctx)

	tx := &InlineCashu{Base: storage.New(storage.ID(c.Data))}
	mutex.LockWithContext(ctx, tx.ID)
	defer mutex.UnlockWithContext(ctx, tx.ID)

	fn, err := tx.Get(tx, bot.Bunt)
	if err != nil {
		log.Errorf("[cancelInlineCashuHandler] Error: %s", err.Error())
		return ctx, err
	}

	inlineCashu := fn.(*InlineCashu)

	// Only the creator can cancel
	if inlineCashu.From.Telegram.ID != user.Telegram.ID {
		ctx.Context = context.WithValue(ctx, "callback_response", Translate(ctx, "cantDoThatMessage"))
		return ctx, errors.Create(errors.InvalidTypeError)
	}

	if !inlineCashu.Active || inlineCashu.Claimed {
		return ctx, errors.Create(errors.NotActiveError)
	}

	// ponytail: nothing to refund — the token is only minted on claim, so an
	// unclaimed cancel never spent the creator's sats.

	// Mark as inactive
	inlineCashu.Active = false
	inlineCashu.Canceled = true
	inlineCashu.Set(inlineCashu, bot.Bunt)

	bot.tryEditMessage(c.Message, Translate(ctx, "cashuSendCancelledMessage"), &tb.ReplyMarkup{})
	log.Infof("[cashu cancel] %s cancelled cashu token %s", GetUserStr(user.Telegram), inlineCashu.ID)
	return ctx, nil
}
