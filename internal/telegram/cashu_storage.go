package telegram

import (
	"encoding/json"
	"fmt"

	"github.com/LightningTipBot/LightningTipBot/internal/cashu"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/buntdb"
)

// Cashu token lifecycle states.
const (
	cashuStateMinting   = "minting"   // user paid the mint invoice; token not yet minted (recoverable via /cashu recover)
	cashuStateUnclaimed = "unclaimed" // token minted and held in the wallet, not yet redeemed
	cashuStateSpent     = "spent"     // token has been redeemed/claimed

	// maxPendingCashuTokens caps minting+unclaimed records per user so a user
	// can't grow the DB and hammer the mint without bound.
	// ponytail: hard-coded; make configurable if a real user ever hits it.
	maxPendingCashuTokens = 10
)

// CashuToken is a durable per-user record of a cashu token the user minted.
// It exists so sats are never stranded in a chat message the user might lose,
// and so a paid-but-not-minted quote can be recovered instead of lost.
type CashuToken struct {
	*storage.Base
	TelegramID int64  `json:"cashu_telegram_id"`
	Username   string `json:"cashu_username"`
	Amount     int64  `json:"cashu_amount"`
	Memo       string `json:"cashu_memo"`
	Token      string `json:"cashu_token"`    // empty while state == minting
	QuoteId    string `json:"cashu_quote_id"` // paid mint quote, used for recovery
	State      string `json:"cashu_state"`
}

func cashuTokenKey(telegramID int64, id string) string {
	return fmt.Sprintf("cashutoken:%d:%s", telegramID, id)
}

// newCashuToken creates a minting-state record. Persist it BEFORE paying the
// mint invoice so a crash/failure between pay and mint is always recoverable.
func newCashuToken(telegramID int64, username string, amount int64, memo, quoteId string) *CashuToken {
	return &CashuToken{
		Base:       storage.New(storage.ID(cashuTokenKey(telegramID, RandStringRunes(10)))),
		TelegramID: telegramID,
		Username:   username,
		Amount:     amount,
		Memo:       memo,
		QuoteId:    quoteId,
		State:      cashuStateMinting,
	}
}

func (bot *TipBot) setCashuToken(c *CashuToken) error {
	return c.Set(c, bot.Bunt)
}

// listCashuTokens returns all cashu token records for a user.
func (bot *TipBot) listCashuTokens(telegramID int64) ([]*CashuToken, error) {
	var tokens []*CashuToken
	prefix := fmt.Sprintf("cashutoken:%d:", telegramID)
	err := bot.Bunt.View(func(tx *buntdb.Tx) error {
		return tx.AscendKeys(prefix+"*", func(key, value string) bool {
			var c CashuToken
			if err := json.Unmarshal([]byte(value), &c); err != nil {
				log.Errorf("[cashu list] corrupt record %s: %s", key, err.Error())
				return true // skip, keep iterating
			}
			tokens = append(tokens, &c)
			return true
		})
	})
	return tokens, err
}

// countPendingCashuTokens returns how many minting/unclaimed records a user has.
func (bot *TipBot) countPendingCashuTokens(telegramID int64) (int, error) {
	tokens, err := bot.listCashuTokens(telegramID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range tokens {
		if c.State == cashuStateMinting || c.State == cashuStateUnclaimed {
			n++
		}
	}
	return n, nil
}

// markCashuTokenSpent flips the caller's stored token matching tokenStr to spent.
// Only matches the user's own minted tokens; external tokens are simply ignored.
func (bot *TipBot) markCashuTokenSpent(telegramID int64, tokenStr string) {
	tokens, err := bot.listCashuTokens(telegramID)
	if err != nil {
		return
	}
	for _, c := range tokens {
		if c.Token != "" && c.Token == tokenStr && c.State != cashuStateSpent {
			c.State = cashuStateSpent
			_ = bot.setCashuToken(c)
			return
		}
	}
}

// refreshCashuTokenState checks an unclaimed token against the mint (NUT-07) and
// marks it spent if its proofs were redeemed elsewhere. Best-effort: if the mint
// doesn't support state checks, the record is left unchanged.
func (bot *TipBot) refreshCashuTokenState(c *CashuToken) {
	if c.State != cashuStateUnclaimed || c.Token == "" {
		return
	}
	token, err := cashu.Deserialize(c.Token)
	if err != nil || len(token.Token) == 0 {
		return
	}
	unspent, err := bot.CashuClient.AllProofsUnspent(token.Token[0].Proofs)
	if err != nil {
		return
	}
	if !unspent {
		c.State = cashuStateSpent
		_ = bot.setCashuToken(c)
	}
}
