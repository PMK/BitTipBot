package telegram

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/buntdb"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

const (
	// How often the background loop scans for stuck mints.
	cashuAutoRecoverInterval = 2 * time.Minute
	// Records younger than this are skipped: their original mint handler may
	// still be inside WaitQuotePaid, and re-driving the same quote from two
	// goroutines would race.
	cashuAutoRecoverMinAge = 5 * time.Minute
)

// cashuAutoRecoverLoop periodically re-drives paid-but-unminted quotes so
// users get their token without knowing about /cashu recover.
func (bot *TipBot) cashuAutoRecoverLoop() {
	if !internal.Configuration.Cashu.Enabled {
		return
	}
	for {
		time.Sleep(cashuAutoRecoverInterval)
		bot.cashuAutoRecoverOnce()
	}
}

// cashuAutoRecoverOnce scans ALL users' cashu records for stuck mints.
func (bot *TipBot) cashuAutoRecoverOnce() {
	var stuck []*CashuToken
	err := bot.Bunt.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys("cashutoken:*", func(key, value string) bool {
			var c CashuToken
			if err := json.Unmarshal([]byte(value), &c); err != nil {
				return true // skip corrupt record, keep going
			}
			if c.State == cashuStateMinting && time.Since(c.CreatedAt) > cashuAutoRecoverMinAge {
				stuck = append(stuck, &c)
			}
			return true
		})
	})
	if err != nil {
		log.Errorf("[cashu autorecover] scan failed: %s", err.Error())
		return
	}

	for _, c := range stuck {
		q, err := bot.CashuClient.CheckMintQuote(c.QuoteId)
		if err != nil {
			log.Warnf("[cashu autorecover] check quote %s: %s", c.QuoteId, err.Error())
			continue
		}
		recipient := &tb.User{ID: c.TelegramID}
		if q.State == "ISSUED" {
			// Signed elsewhere, blinding data gone — not re-derivable. Stop retrying.
			c.State = cashuStateSpent
			_ = bot.setCashuToken(c)
			log.Warnf("[cashu autorecover] quote %s already issued, marking spent", c.QuoteId)
			continue
		}
		if !q.IsPaid() {
			if q.Expiry > 0 && time.Now().Unix() > q.Expiry {
				// Invoice never paid and quote expired: nothing was spent. Close it.
				_ = c.Inactivate(c, bot.Bunt)
				log.Infof("[cashu autorecover] quote %s expired unpaid, closing record", c.QuoteId)
			}
			continue
		}
		tokenStr, err := bot.CashuClient.MintTokens(c.QuoteId, c.Amount, c.Memo)
		if err != nil {
			log.Errorf("[cashu autorecover] mint from quote %s failed: %s", c.QuoteId, err.Error())
			continue
		}
		c.Token = tokenStr
		c.State = cashuStateUnclaimed
		_ = bot.setCashuToken(c)
		bot.trySendMessage(recipient, fmt.Sprintf("🥜 Your pending *%d sat* cashu token was recovered automatically:\n`%s`", c.Amount, tokenStr))
		log.Infof("[cashu autorecover] recovered %d sat for user %d (quote %s)", c.Amount, c.TelegramID, c.QuoteId)
	}
}
