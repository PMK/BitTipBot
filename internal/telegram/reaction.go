package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/telegram/intercept"
	"github.com/eko/gocache/store"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// handleReaction processes a message_reaction update parsed from raw JSON.
// It determines which emoji was added, looks up reactor and message author,
// and executes a tip if the reactor has configured a reaction amount.
func (bot *TipBot) handleReaction(reaction gjson.Result) {
	chatID := reaction.Get("chat.id").Int()
	messageID := reaction.Get("message_id").Int()
	reactorID := reaction.Get("user.id").Int()
	reactorFirstName := reaction.Get("user.first_name").String()
	reactorUsername := reaction.Get("user.username").String()

	if chatID == 0 || messageID == 0 || reactorID == 0 {
		return
	}

	// Determine which emoji was added by diffing new_reaction vs old_reaction
	emoji := diffReactionEmoji(reaction)
	if emoji == "" {
		return
	}

	// Only process thumbs-up and heart reactions
	// Telegram sends "❤" (U+2764), not "❤️" (U+2764+FE0F)
	normalizedEmoji := normalizeEmoji(emoji)
	if normalizedEmoji != "👍" && normalizedEmoji != "❤" && normalizedEmoji != "⚡" {
		return
	}

	// Rate limit: prevent double-tips from rapid reaction toggling
	dedupKey := fmt.Sprintf("reactiontip:%d:%d:%d", reactorID, chatID, messageID)
	if _, err := bot.Cache.Get(dedupKey); err == nil {
		return // already processed recently
	}
	bot.Cache.Set(dedupKey, true, &store.Options{Expiration: 10 * time.Second})

	// Look up reactor user and their reaction settings
	reactorTbUser := &tb.User{
		ID:        reactorID,
		FirstName: reactorFirstName,
		Username:  reactorUsername,
	}
	fromUser, err := GetLnbitsUserWithSettings(reactorTbUser, *bot)
	if err != nil {
		log.Debugf("[handleReaction] reactor %d not found: %v", reactorID, err)
		return
	}

	// Check the configured tip amount for this emoji
	var amount int64
	switch normalizedEmoji {
	case "👍":
		amount = fromUser.Settings.Reaction.ThumbsUpAmount
	case "❤":
		amount = fromUser.Settings.Reaction.HeartAmount
	case "⚡":
		amount = fromUser.Settings.Reaction.ThunderAmount
	}
	if amount <= 0 {
		return
	}

	// Look up the message author from cache
	authorCacheKey := fmt.Sprintf("msgauthor:%d:%d", chatID, messageID)
	cached, err := bot.Cache.Get(authorCacheKey)
	if err != nil {
		log.Debugf("[handleReaction] cache miss for message %d in chat %d", messageID, chatID)
		return // cache miss, silently return for v1
	}
	authorTbUser, ok := cached.(*tb.User)
	if !ok || authorTbUser == nil {
		return
	}

	// Prevent self-tips
	if reactorID == authorTbUser.ID {
		return
	}
	// Prevent tipping the bot
	if bot.Telegram.Me != nil && authorTbUser.ID == bot.Telegram.Me.ID {
		return
	}

	// Get or create recipient user
	toUser, exists := bot.UserExists(authorTbUser)
	if !exists {
		toUser, err = bot.CreateWalletForTelegramUser(authorTbUser)
		if err != nil {
			log.Errorf("[handleReaction] could not create wallet for %s: %v", GetUserStr(authorTbUser), err)
			return
		}
	}

	fromUserStr := GetUserStr(fromUser.Telegram)
	toUserStr := GetUserStr(toUser.Telegram)
	fromUserStrMd := GetUserStrMd(fromUser.Telegram)
	toUserStrMd := GetUserStrMd(toUser.Telegram)

	// Execute the tip
	transactionMemo := fmt.Sprintf("%s Reaction tip from %s to %s.", normalizedEmoji, fromUserStr, toUserStr)
	chat := &tb.Chat{ID: chatID}
	t := NewTransaction(bot, fromUser, toUser, amount, TransactionType("reaction"), TransactionChat(chat))
	t.Memo = transactionMemo
	success, err := t.Send()
	if !success {
		log.Warnf("[handleReaction] transaction failed from %s to %s: %v", fromUserStr, toUserStr, err)
		bot.trySendMessage(fromUser.Telegram, fmt.Sprintf("Could not send %s reaction tip: %v", normalizedEmoji, err))
		return
	}

	log.Infof("[💸 reaction] %s tip from %s to %s (%d sat).", normalizedEmoji, fromUserStr, toUserStr, amount)

	// Notify both users
	bot.trySendMessage(fromUser.Telegram, fmt.Sprintf("%s Reaction tip of %d sat sent to %s.", normalizedEmoji, amount, toUserStrMd))
	bot.trySendMessage(toUser.Telegram, fmt.Sprintf("%s Reaction tip of %d sat received from %s.", normalizedEmoji, amount, fromUserStrMd))
}

// diffReactionEmoji determines which emoji was added by comparing
// new_reaction and old_reaction arrays. Returns the first emoji
// present in new_reaction but not in old_reaction.
func diffReactionEmoji(reaction gjson.Result) string {
	newReactions := reaction.Get("new_reaction").Array()
	oldReactions := reaction.Get("old_reaction").Array()

	oldSet := make(map[string]bool)
	for _, r := range oldReactions {
		emoji := r.Get("emoji").String()
		if emoji != "" {
			oldSet[emoji] = true
		}
	}

	for _, r := range newReactions {
		emoji := r.Get("emoji").String()
		if emoji != "" && !oldSet[emoji] {
			return emoji
		}
	}
	return ""
}

// normalizeEmoji normalizes heart emoji variants.
// Telegram sends "❤" (U+2764), but users might also use "❤️" (U+2764+FE0F).
func normalizeEmoji(emoji string) string {
    // Strip variation selector FE0F
    normalized := strings.TrimRight(emoji, "\uFE0F")
    return normalized
}

// reactionHandler handles the /reaction command (DM only).
// Usage:
//
//	/reaction          — show current settings
//	/reaction 👍 100   — set thumbs-up tip to 100 sats
//	/reaction ❤ 50    — set heart tip to 50 sats
//	/reaction 👍 0    — disable thumbs-up tipping
func (bot *TipBot) reactionHandler(ctx intercept.Context) (intercept.Context, error) {
	m := ctx.Message()
	user, err := GetLnbitsUserWithSettings(m.Sender, *bot)
	if err != nil {
		return ctx, err
	}

	splits := strings.Fields(m.Text)

	// /reaction — show current settings
	if len(splits) == 1 {
		thumbsUp := user.Settings.Reaction.ThumbsUpAmount
		heart := user.Settings.Reaction.HeartAmount
		thunder := user.Settings.Reaction.ThunderAmount
		msg := fmt.Sprintf("*Reaction tip settings:*\n👍 = %d sat\n❤ = %d sat\n⚡ = %d sat\n\nUse `/reaction 👍 100` to set amounts.", thumbsUp, heart, thunder)
		bot.trySendMessage(m.Sender, msg)
		return ctx, nil
	}

	// /reaction <emoji> <amount>
	if len(splits) < 3 {
		bot.trySendMessage(m.Sender, "Usage: `/reaction 👍 100` or `/reaction ❤ 50` or `/reaction ⚡ 50`")
		return ctx, nil
	}

	emoji := normalizeEmoji(splits[1])
	if emoji != "👍" && emoji != "❤" && emoji != "⚡" {
		bot.trySendMessage(m.Sender, "Supported emojis: 👍 and ❤ and ⚡")
		return ctx, nil
	}

	amount, err := strconv.ParseInt(splits[2], 10, 64)
	if err != nil || amount < 0 {
		bot.trySendMessage(m.Sender, "Please enter a valid amount (0 to disable).")
		return ctx, nil
	}

	switch emoji {
	case "👍":
		user.Settings.Reaction.ThumbsUpAmount = amount
	case "❤":
		user.Settings.Reaction.HeartAmount = amount
	case "⚡":
		user.Settings.Reaction.ThunderAmount = amount
	}

	err = UpdateUserRecord(user, *bot)
	if err != nil {
		log.Errorf("[reactionHandler] could not update record of user %s: %v", GetUserStr(user.Telegram), err)
		return ctx, err
	}

	if amount == 0 {
		bot.trySendMessage(m.Sender, fmt.Sprintf("Disabled %s reaction tipping.", emoji))
	} else {
		bot.trySendMessage(m.Sender, fmt.Sprintf("Set %s reaction tip to %d sat.", emoji, amount))
	}
	return ctx, nil
}
