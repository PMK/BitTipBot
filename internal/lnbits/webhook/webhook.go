package webhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/telegram"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"

	"net/http"

	"github.com/LightningTipBot/LightningTipBot/internal/storage"

	"github.com/gorilla/mux"
	tb "gopkg.in/lightningtipbot/telebot.v3"

	"github.com/LightningTipBot/LightningTipBot/internal/i18n"
)

type Server struct {
	httpServer *http.Server
	bot        *tb.Bot
	c          *lnbits.Client
	database   *gorm.DB
	buntdb     *storage.DB
}

type Webhook struct {
	CheckingID    string      `json:"checking_id"`
	Pending       bool        `json:"pending"`
	Amount        int64       `json:"amount"`
	Fee           int64       `json:"fee"`
	Memo          string      `json:"memo"`
	Time          int64       `json:"time"`
	Bolt11        string      `json:"bolt11"`
	Preimage      string      `json:"preimage"`
	PaymentHash   string      `json:"payment_hash"`
	Extra         struct{}    `json:"extra"`
	WalletID      string      `json:"wallet_id"`
	Webhook       string      `json:"webhook"`
	WebhookStatus interface{} `json:"webhook_status"`
}

func NewServer(bot *telegram.TipBot) *Server {
	srv := &http.Server{
		Addr:         internal.Configuration.Lnbits.WebhookServerUrl.Host,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}
	apiServer := &Server{
		c:          bot.Client,
		database:   bot.DB.Users,
		bot:        bot.Telegram,
		httpServer: srv,
		buntdb:     bot.Bunt,
	}
	apiServer.httpServer.Handler = apiServer.newRouter()
	go apiServer.httpServer.ListenAndServe()
	log.Infof("[Webhook] Server started at %s", internal.Configuration.Lnbits.WebhookServerUrl)
	return apiServer
}

// GetUserByWalletId looks up a user by their wallet ID.
// The Wallet is embedded in the User struct with GORM prefix "wallet_", so the
// column for Wallet.ID in the users table is "wallet_id".
func (w *Server) GetUserByWalletId(walletId string) (*lnbits.User, error) {
	user := &lnbits.User{}
	tx := w.database.Where("wallet_id = ?", walletId).First(user)
	if tx.Error != nil {
		return user, tx.Error
	}
	return user, nil
}

func (w *Server) newRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", w.receive).Methods(http.MethodPost)
	return router
}

func (w *Server) receive(writer http.ResponseWriter, request *http.Request) {
	log.Debugln("[Webhook] Received request")
	webhookEvent := Webhook{}
	// Strip content-length header to prevent Decode failures on some clients
	request.Header.Del("content-length")
	err := json.NewDecoder(request.Body).Decode(&webhookEvent)
	if err != nil {
		log.Errorf("[Webhook] Error decoding request: %s", err.Error())
		writer.WriteHeader(400)
		return
	}

	// Ignore pending (unpaid) payments — only process settled payments
	if webhookEvent.Pending {
		writer.WriteHeader(200)
		return
	}

	user, err := w.GetUserByWalletId(webhookEvent.WalletID)
	if err != nil {
		log.Errorf("[Webhook] Error getting user for wallet_id %s: %s", webhookEvent.WalletID, err.Error())
		writer.WriteHeader(400)
		return
	}

	log.Infoln(fmt.Sprintf("[⚡️ WebHook] User %s (%d) received invoice of %d sat.",
		telegram.GetUserStr(user.Telegram), user.Telegram.ID, webhookEvent.Amount/1000))

	// Always respond 200 immediately so LNbits doesn't retry
	writer.WriteHeader(200)

	// Try to find and fire a registered invoice callback (e.g. from LNURL-pay or lightning address)
	txInvoiceEvent := &telegram.InvoiceEvent{Invoice: &telegram.Invoice{PaymentHash: webhookEvent.PaymentHash}}
	err = w.buntdb.Get(txInvoiceEvent)
	if err == nil {
		// An invoice event was registered for this payment hash
		c := telegram.InvoiceCallback[txInvoiceEvent.Callback]
		if c.Function != nil {
			// Validate the event type before dispatching
			if assertErr := telegram.AssertEventType(txInvoiceEvent, c.Type); assertErr != nil {
				log.Errorln(assertErr)
				// Fall through to send the generic notification below
			} else {
				go c.Function(txInvoiceEvent)
				return
			}
		}
		// Callback key was registered but no handler found — fall through to generic notification
		log.Warnf("[Webhook] No callback handler for invoice event callback=%d, payment_hash=%s",
			txInvoiceEvent.Callback, webhookEvent.PaymentHash)
	}

	// Fallback: send a plain "you received X sat" message to the user
	_, sendErr := w.bot.Send(
		user.Telegram,
		fmt.Sprintf(i18n.Translate(user.Telegram.LanguageCode, "invoiceReceivedMessage"),
			webhookEvent.Amount/1000),
	)
	if sendErr != nil {
		log.Errorf("[Webhook] Failed to send notification to user %s: %s",
			telegram.GetUserStr(user.Telegram), sendErr.Error())
	}
}
