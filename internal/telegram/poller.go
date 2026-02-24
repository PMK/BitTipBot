package telegram

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/eko/gocache/store"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

// ReactionPoller is a custom poller that extends LongPoller to support
// message_reaction updates (Telegram Bot API 7.0+). The telebot library
// fork used by this project predates that API version, so we handle
// reaction updates manually via raw JSON parsing.
type ReactionPoller struct {
	Timeout      time.Duration
	LastUpdateID int
	bot          *TipBot
}

// Poll implements the tb.Poller interface. It calls getUpdates with
// allowed_updates including "message_reaction", parses the raw JSON,
// and routes standard updates through the channel while handling
// reaction updates directly.
func (p *ReactionPoller) Poll(b *tb.Bot, dest chan tb.Update, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		updates, err := p.getUpdates(b)
		if err != nil {
			log.Errorf("[ReactionPoller] getUpdates error: %v", err)
			time.Sleep(time.Second)
			continue
		}

		results := gjson.GetBytes(updates, "result")
		if !results.Exists() {
			continue
		}

		for _, raw := range results.Array() {
			updateID := int(raw.Get("update_id").Int())
			p.LastUpdateID = updateID

			// Cache message author for any group message
			p.cacheMessageAuthor(raw)

			// Check if this is a message_reaction update
			if raw.Get("message_reaction").Exists() {
				if p.bot != nil {
					go p.bot.handleReaction(raw.Get("message_reaction"))
				}
				continue
			}

			// Standard update: unmarshal into tb.Update and pass through
			var update tb.Update
			if err := json.Unmarshal([]byte(raw.Raw), &update); err != nil {
				log.Errorf("[ReactionPoller] unmarshal update error: %v", err)
				continue
			}
			dest <- update
		}
	}
}

// getUpdates calls the Telegram getUpdates API with allowed_updates
// including message_reaction.
func (p *ReactionPoller) getUpdates(b *tb.Bot) ([]byte, error) {
	params := map[string]string{
		"offset":  strconv.Itoa(p.LastUpdateID + 1),
		"timeout": strconv.Itoa(int(p.Timeout / time.Second)),
		"allowed_updates": toJSON([]string{
			"message",
			"edited_message",
			"channel_post",
			"callback_query",
			"inline_query",
			"chosen_inline_result",
			"message_reaction",
		}),
	}
	return b.Raw("getUpdates", params)
}

// cacheMessageAuthor stores the sender of group messages so we can look up
// the author when a reaction arrives (reaction updates don't include message author).
func (p *ReactionPoller) cacheMessageAuthor(raw gjson.Result) {
	msg := raw.Get("message")
	if !msg.Exists() {
		return
	}
	chat := msg.Get("chat")
	chatType := chat.Get("type").String()
	if chatType != "group" && chatType != "supergroup" {
		return
	}
	from := msg.Get("from")
	if !from.Exists() {
		return
	}
	chatID := chat.Get("id").Int()
	msgID := msg.Get("message_id").Int()
	senderID := from.Get("id").Int()
	firstName := from.Get("first_name").String()
	username := from.Get("username").String()

	if p.bot == nil || senderID == 0 {
		return
	}

	cacheKey := fmt.Sprintf("msgauthor:%d:%d", chatID, msgID)
	author := &tb.User{
		ID:        senderID,
		FirstName: firstName,
		Username:  username,
	}
	p.bot.Cache.Set(cacheKey, author, &store.Options{Expiration: 24 * time.Hour})
}

func toJSON(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}
