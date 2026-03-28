package telegram

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/LightningTipBot/LightningTipBot/internal/runtime/mutex"

	limiter "github.com/LightningTipBot/LightningTipBot/internal/rate"

	"github.com/eko/gocache/store"

	"github.com/LightningTipBot/LightningTipBot/internal"
	"github.com/LightningTipBot/LightningTipBot/internal/lnbits"
	"github.com/LightningTipBot/LightningTipBot/internal/storage"
	gocache "github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
	tb "gopkg.in/lightningtipbot/telebot.v3"
)

type TipBot struct {
	DB       *Databases
	Bunt     *storage.DB
	ShopBunt *storage.DB
	Telegram *tb.Bot
	Client   *lnbits.Client
	limiter  map[string]limiter.Limiter
	Cache
}
type Cache struct {
	*store.GoCacheStore
}

var (
	botWalletInitialisation     = sync.Once{}
	telegramHandlerRegistration = sync.Once{}
)

// NewBot migrates data and creates a new bot
func NewBot() TipBot {
	gocacheClient := gocache.New(5*time.Minute, 10*time.Minute)
	gocacheStore := store.NewGoCache(gocacheClient, nil)
	// create sqlite databases
	dbs := AutoMigration()
	limiter.Start()
	poller := &ReactionPoller{Timeout: 60 * time.Second}
	bot := TipBot{
		DB:       dbs,
		Client:   lnbits.NewClient(internal.Configuration.Lnbits.AdminKey, internal.Configuration.Lnbits.Url),
		Bunt:     createBunt(internal.Configuration.Database.BuntDbPath),
		ShopBunt: createBunt(internal.Configuration.Database.ShopBuntDbPath),
		Telegram: newTelegramBot(poller),
		Cache:    Cache{GoCacheStore: gocacheStore},
	}
	poller.bot = &bot
	return bot
}

// newTelegramBot will create a new Telegram bot.
func newTelegramBot(poller tb.Poller) *tb.Bot {
	tgb, err := tb.NewBot(tb.Settings{
		Token:     internal.Configuration.Telegram.ApiKey,
		Poller:    poller,
		ParseMode: tb.ModeMarkdown,
		Verbose:   false,
	})
	if err != nil {
		panic(err)
	}
	return tgb
}

// initBotWallet will create / initialize the bot wallet
func (bot TipBot) initBotWallet() error {
	botWalletInitialisation.Do(func() {
		_, err := bot.initWallet(bot.Telegram.Me)
		if err != nil {
			log.Errorln(fmt.Sprintf("[initBotWallet] Could not initialize bot wallet: %s", err.Error()))
			return
		}
	})
	return nil
}

// GracefulShutdown will gracefully shutdown the bot
// It will wait for all mutex locks to unlock before shutdown.
func (bot *TipBot) GracefulShutdown() {
	t := time.NewTicker(time.Second * 10)
	log.Infof("[shutdown] Graceful shutdown (timeout=10s).")
	for {
		select {
		case <-t.C:
			// timer expired
			log.Infof("[shutdown] Graceful shutdown timeout reached. Forcing shutdown.")
			return
		default:
			// check if all mutex locks are unlocked
			if mutex.IsEmpty() {
				log.Infof("[shutdown] Graceful shutdown successful.")
				return
			}
		}
		time.Sleep(time.Second)
		log.Tracef("[shutdown] Trying graceful shutdown...")
	}
}

// Start will initialize the Telegram bot and lnbits.
func (bot *TipBot) Start() {
	log.Infof("[Telegram] Authorized on account @%s", bot.Telegram.Me.Username)
	// initialize the bot wallet
	err := bot.initBotWallet()
	if err != nil {
		log.Errorf("Could not initialize bot wallet: %s", err.Error())
	}

	// register telegram handlers
	bot.registerTelegramHandlers()

	// download bot avatar once
	bot.downloadMyProfilePicture()

	// edit worker collects messages to edit and
	// periodically edits them
	bot.startEditWorker()

	// register callbacks for invoices
	initInvoiceEventCallbacks(bot)

	// register callbacks for user state changes
	initializeStateCallbackMessage(bot)

	// start the telegram bot
	go bot.Telegram.Start()

	go bot.restartPersistedTickets()

	// gracefully shutdown on interrupt or termination signal.
	// syscall.SIGSTOP is not catchable on any platform and is not defined on
	// Windows, so it must not be passed to signal.Notify.
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM)
	<-exit
	bot.GracefulShutdown()
}
