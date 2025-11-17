package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fiatjaf/go-lnurl"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"

	"github.com/LightningTipBot/LightningTipBot/internal/errors"

	"github.com/LightningTipBot/LightningTipBot/internal/str"

	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// This file has been simplified so that all donations initiated through
// the bot are forwarded to a fixed lightning address: kevinrav@btip.nl
// (or the equivalent user @kevinrav). The behaviour is intentionally
// straightforward: resolve the LN address to an LNURL pay endpoint,
// request an invoice for the requested amount and pay it from the
// user's wallet.

const fixedLightningAddress = "kevinrav@btip.nl"

func helpDonateUsage(ctx context.Context, errormsg string) string {
	if len(errormsg) > 0 {
		return fmt.Sprintf(Translate(ctx, "donateHelpText"), fmt.Sprintf("%s", errormsg))
	} else {
		return fmt.Sprintf(Translate(ctx, "donateHelpText"), "")
	}
}

func (bot TipBot) donationHandler(ctx intercept.Context) (intercept.Context, error) {
	// preserve existing behaviour that logs any text and loads user
	m := ctx.Message()
	bot.anyTextHandler(ctx)
	user := LoadUser(ctx)
	if user.Wallet == nil {
		return ctx, errors.Create(errors.UserNoWalletError)
	}

	// decode amount from command; if none and private chat, ask for it
	amount, err := decodeAmountFromCommand(m.Text)
	if (err != nil || amount < 1) && m.Chat.Type == tb.ChatPrivate {
		_, err = bot.askForAmount(ctx, "", "CreateDonationState", 0, 0, m.Text)
		return ctx, err
	}
	// convert sats -> millisats (existing behaviour)
	amount = amount * 1000

	// send progress message
	msg := bot.trySendMessageEditable(m.Chat, Translate(ctx, "donationProgressMessage"))

	// Resolve fixed lightning address to LNURL pay endpoint:
	parts := strings.Split(fixedLightningAddress, "@")
	if len(parts) != 2 {
		log.Errorln("invalid fixed lightning address:", fixedLightningAddress)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, fmt.Errorf("invalid fixed lightning address")
	}
	userPart := parts[0]
	domainPart := parts[1]

	lnurlEndpoint := fmt.Sprintf("https://%s/.well-known/lnurlp/%s", domainPart, userPart)

	// Create request with amount and a short comment
	req, err := http.NewRequest(http.MethodGet, lnurlEndpoint, nil)
	if err != nil {
		log.Errorln(err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}
	q := url.Values{}
	q.Set("amount", strconv.FormatInt(amount, 10))
	q.Set("comment", fmt.Sprintf("from %s via bot %s", GetUserStr(user.Telegram), GetUserStr(bot.Telegram.Me)))
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Errorln(err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Errorln(err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}

	pv := lnurl.LNURLPayValues{}
	err = json.Unmarshal(body, &pv)
	if err != nil {
		log.Errorln(err)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}
	if pv.Status == "ERROR" || len(pv.PR) < 1 {
		log.Errorln("lnurl pay endpoint returned an error or no invoice:", string(body))
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, fmt.Errorf("lnurl pay endpoint error")
	}

	// pay the returned invoice
	_, err = user.Wallet.Pay(lnbits.PaymentParams{Out: true, Bolt11: string(pv.PR)}, bot.Client)
	if err != nil {
		userStr := GetUserStr(user.Telegram)
		errmsg := fmt.Sprintf("[/donate] Donation failed for user %s: %s", userStr, err)
		log.Errorln(errmsg)
		bot.tryEditMessage(msg, Translate(ctx, "donationErrorMessage"))
		return ctx, err
	}

	// remove progress and notify success
	bot.tryDeleteMessage(msg)
	bot.trySendMessage(m.Chat, Translate(ctx, "donationSuccess"))
	return ctx, nil
}

func (bot TipBot) parseCmdDonHandler(ctx intercept.Context) error {
	m := ctx.Message()

	// If this message is a reply to another user's message (a post tip), do not
	// convert it into a /donate action. Donations to the fixed lightning address
	// should only happen for explicit donation commands, not when tipping other users' posts.
	if m.ReplyTo != nil && m.ReplyTo.Sender != nil && m.Sender != nil && m.ReplyTo.Sender.ID != m.Sender.ID {
		// Do not intercept — allow original /tip or /send flow to continue.
		// We return a non-nil error to indicate we skipped donation conversion.
		// The caller of this handler should treat non-nil as "did not handle".
		return fmt.Errorf("skipped donation conversion for reply-to-other-user")
	}
	
	// SAFETY CHECK -- only rewrite message if the original command really was /donate
	if !strings.HasPrefix(m.Text, "/donate") {
		return fmt.Errorf("not a donation command; skipping donation handler")
	}
	
	// try to extract amount (if not present, decodeAmountFromCommand will return error)
	amount, err := decodeAmountFromCommand(m.Text)
	if err != nil || amount < 1 {
		// if we don't have an amount, let the donation handler ask for it
		amount = 0
	}

	// Inform the user that the donation will be forwarded to the fixed recipient
	notice := fmt.Sprintf("Thanks — donations initiated here will be forwarded to @kevinrav (%s).", fixedLightningAddress)
	bot.trySendMessage(m.Sender, str.MarkdownEscape(notice))

	// rewrite message to call /donate with the detected amount (or with no amount so donateHandler asks)
	if amount > 0 {
		m.Text = fmt.Sprintf("/donate %d", amount)
	} else {
		m.Text = "/donate"
	}
	// call donation handler which will perform the actual transfer
	_, err = bot.donationHandler(ctx)
	if err != nil {
		log.Errorln("donation handler returned error:", err)
	}

	// returning nil will abort the parent context (so original /tip or /send stops)
	return nil
}
