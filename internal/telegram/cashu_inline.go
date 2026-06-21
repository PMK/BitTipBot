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

	// Mint the token
	quote, err := bot.CashuClient.MintQuote(amount, "sat")
	if err != nil {
		log.Errorf("[cashu inline] MintQuote error: %s", err.Error())
		bot.inlineQueryReplyWithError(ctx, "Mint error", "Could not communicate with the cashu mint.")
		return ctx, err
	}

	// Pay the mint's invoice
	_, err = fromUser.Wallet.Pay(lnbits.PaymentParams{
		Out:    true,
		Bolt11: quote.Request,
	}, bot.Client)
	if err != nil {
		log.Errorf("[cashu inline] Payment to mint failed: %s", err.Error())
		bot.inlineQueryReplyWithError(ctx, "Payment error", "Could not pay the mint's invoice.")
		return ctx, err
	}

	time.Sleep(2 * time.Second)

	tokenStr, err := bot.CashuClient.MintTokens(quote.Quote, amount, memo)
	if err != nil {
		log.Errorf("[cashu inline] MintTokens failed: %s", err.Error())
		bot.inlineQueryReplyWithError(ctx, "Mint error", "Could not create ecash token.")
		return ctx, err
	}

	// Create the inline cashu object
	id := fmt.Sprintf("cashu:%s:%d", RandStringRunes(10), amount)
	inlineMessage := fmt.Sprintf(Translate(ctx, "cashuSendMessage"), GetUserStrMd(query.Sender), amount)
	if len(memo) > 0 {
		inlineMessage += fmt.Sprintf("\n_Memo: %s_", memo)
	}

	inlineCashu := &InlineCashu{
		Base:         storage.New(storage.ID(id)),
		Message:      inlineMessage,
		Amount:       amount,
		From:         fromUser,
		Token:        tokenStr,
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

	// Redeem the token
	token, err := cashu.Deserialize(inlineCashu.Token)
	if err != nil {
		log.Errorf("[cashu claim] Invalid stored token: %s", err.Error())
		return ctx, err
	}

	proofs := token.Token[0].Proofs
	totalAmount := token.Amount()

	// Create invoice on recipient's wallet
	invoice, err := to.Wallet.Invoice(lnbits.InvoiceParams{
		Out:    false,
		Amount: totalAmount,
		Memo:   fmt.Sprintf("Cashu ecash claim (%d sat)", totalAmount),
	}, bot.Client)
	if err != nil {
		log.Errorf("[cashu claim] Failed to create invoice: %s", err.Error())
		return ctx, err
	}

	// Melt at the mint
	meltQuote, err := bot.CashuClient.MeltQuote(invoice.PaymentRequest, "sat")
	if err != nil {
		log.Errorf("[cashu claim] MeltQuote failed: %s", err.Error())
		return ctx, err
	}

	meltResp, err := bot.CashuClient.Melt(meltQuote.Quote, proofs)
	if err != nil {
		log.Errorf("[cashu claim] Melt failed: %s", err.Error())
		return ctx, err
	}

	if meltResp.State != "PAID" {
		log.Warnf("[cashu claim] Melt not paid, state: %s", meltResp.State)
		return ctx, fmt.Errorf("melt state: %s", meltResp.State)
	}

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

	// Redeem the token back to the creator's wallet
	token, err := cashu.Deserialize(inlineCashu.Token)
	if err == nil && len(token.Token) > 0 && len(token.Token[0].Proofs) > 0 {
		// Try to redeem back to creator
		proofs := token.Token[0].Proofs
		totalAmount := token.Amount()

		invoice, err := user.Wallet.Invoice(lnbits.InvoiceParams{
			Out:    false,
			Amount: totalAmount,
			Memo:   "Cashu token cancel refund",
		}, bot.Client)
		if err == nil {
			meltQuote, err := bot.CashuClient.MeltQuote(invoice.PaymentRequest, "sat")
			if err == nil {
				bot.CashuClient.Melt(meltQuote.Quote, proofs)
			}
		}
	}

	// Mark as inactive
	inlineCashu.Active = false
	inlineCashu.Canceled = true
	inlineCashu.Set(inlineCashu, bot.Bunt)

	bot.tryEditMessage(c.Message, Translate(ctx, "cashuSendCancelledMessage"), &tb.ReplyMarkup{})
	log.Infof("[cashu cancel] %s cancelled cashu token %s", GetUserStr(user.Telegram), inlineCashu.ID)
	return ctx, nil
}
