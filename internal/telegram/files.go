package telegram

import (
	"github.com/LightningTipBot/LightningTipBot/internal/errors"
	"github.com/LightningTipBot/LightningTipBot/internal/runtime"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	tb "gopkg.in/lightningtipbot/telebot.v3"
	"time"
)

func (bot *TipBot) fileHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	if m.Chat.Type != tb.ChatPrivate {
		return ctx, errors.Create(errors.NoPrivateChatError)
	}
	user := LoadUser(ctx)
	if c := stateCallbackMessage[user.StateKey]; c != nil {
		// found ctx for this state
		// now looking for user state reset ticker
		ticker := runtime.GetFunction(user.ID, runtime.WithTicker(time.NewTicker(runtime.DefaultTickerDuration)))
		if !ticker.Started {
			ticker.Do(func() {
				ResetUserState(user, bot)
				// removing ticker asap done
				bot.shopViewDeleteAllStatusMsgs(ctx, user)
				runtime.RemoveTicker(user.ID)
			})
		} else {
			ticker.ResetChan <- struct{}{}
		}

		return c(ctx)
	}

	// No state waiting for a file: an image document may be a QR code
	// (large cashu token QRs only survive Telegram uncompressed, as files).
	// NOTE: tb.OnDocument is registered exactly once, on this handler —
	// telebot keeps one handler per endpoint, last registration wins.
	if m.Document != nil {
		return bot.documentHandler(ctx)
	}
	return ctx, errors.Create(errors.NoFileFoundError)
}
