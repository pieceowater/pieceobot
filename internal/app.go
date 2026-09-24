// Package internal wires everything together -- mirrors
// lotof.tg.capital.bot's internal/app.go: one place that knows how every
// service connects, Start()/Stop() as the only public surface main.go
// touches.
package internal

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"pieceobot/internal/core/cfg"
	debouncesvc "pieceobot/internal/pkg/debounce/svc"
	limitersvc "pieceobot/internal/pkg/limiter/svc"
	llmsvc "pieceobot/internal/pkg/llm/svc"
	"pieceobot/internal/pkg/storage/repo"
	telegramsvc "pieceobot/internal/pkg/telegram/svc"
)

// Explicit on purpose (ТЗ 2) -- the library can infer this, but the
// business update types are easy to silently lose track of.
var allowedUpdates = tgbot.AllowedUpdates{
	"message",
	"business_connection",
	"business_message",
	"edited_business_message",
	"deleted_business_messages",
}

const cleanupInterval = 24 * time.Hour

type App struct {
	logger   *slog.Logger
	cfg      *cfg.Config
	bot      *tgbot.Bot
	db       *repo.MessagesRepo
	debounce *debouncesvc.Service
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewApp() *App {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	c := cfg.Inst()

	sqlDB, err := repo.Connect(c.DBPath)
	if err != nil {
		logger.Error("failed to open database", slog.Any("error", err))
		os.Exit(1)
	}

	messagesRepo := repo.NewMessagesRepo(sqlDB)
	chatStateRepo := repo.NewChatStateRepo(sqlDB)
	settingsRepo := repo.NewSettingsRepo(sqlDB)
	businessConnRepo := repo.NewBusinessConnectionsRepo(sqlDB)
	llmCallsRepo := repo.NewLlmCallsRepo(sqlDB)

	limiter := limitersvc.New(llmCallsRepo, settingsRepo, c)
	debounce := debouncesvc.New(c.DebounceSec, c.DebounceMaxSec)

	llm, err := llmsvc.New(c, logger)
	if err != nil {
		logger.Error("failed to init llm service", slog.Any("error", err))
		os.Exit(1)
	}

	// The bot's own numeric id is the token prefix -- no GetMe() round trip needed.
	botUserID := botIDFromToken(c.BotToken)

	telegram := telegramsvc.New(c, logger, messagesRepo, chatStateRepo, businessConnRepo, limiter, debounce, llm, botUserID)

	b, err := tgbot.New(c.BotToken,
		tgbot.WithDefaultHandler(telegram.HandleUpdate),
		tgbot.WithAllowedUpdates(allowedUpdates),
		tgbot.WithErrorsHandler(func(err error) {
			logger.Error("bot error", slog.Any("error", err))
		}),
	)
	if err != nil {
		logger.Error("failed to create bot", slog.Any("error", err))
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &App{
		logger: logger, cfg: c, bot: b, db: messagesRepo, debounce: debounce,
		ctx: ctx, cancel: cancel,
	}
}

// ownerCommands is the "/" menu Telegram shows when the owner opens their
// direct chat with the bot -- see telegram/svc/commands.go for the handlers.
var ownerCommands = []models.BotCommand{
	{Command: "pause", Description: "Поставить бота на паузу (глобально)"},
	{Command: "resume", Description: "Снять с паузы"},
	{Command: "mute", Description: "Замьютить чат: /mute <user_id> или ответом на уведомление"},
	{Command: "unmute", Description: "Размьютить чат"},
	{Command: "budget", Description: "Посмотреть/поменять дневной бюджет: /budget <usd>"},
	{Command: "stats", Description: "Статистика: ответы, токены, расход сегодня/за месяц"},
}

// Start blocks until Stop cancels it (graceful shutdown, ТЗ 6).
func (a *App) Start() {
	go a.cleanupLoop()
	a.registerOwnerCommands()
	a.logger.Info("pieceobot started", slog.Int64("owner_user_id", a.cfg.OwnerUserID))
	a.bot.Start(a.ctx)
	a.logger.Info("pieceobot stopped")
}

// registerOwnerCommands sets the "/" command menu, scoped to just the
// owner's private chat with the bot -- nobody else who messages the bot
// directly sees these, even though the handlers themselves also check
// OwnerUserID (belt and suspenders, ТЗ 3.5).
func (a *App) registerOwnerCommands() {
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	_, err := a.bot.SetMyCommands(ctx, &tgbot.SetMyCommandsParams{
		Commands: ownerCommands,
		Scope:    &models.BotCommandScopeChat{ChatID: a.cfg.OwnerUserID},
	})
	if err != nil {
		a.logger.Warn("failed to register owner command menu", slog.Any("error", err))
	}
}

func (a *App) Stop() {
	a.cancel()
	a.debounce.Shutdown()
}

// cleanupLoop deletes messages older than HistoryTTLDays once a day (ТЗ 5).
func (a *App) cleanupLoop() {
	a.runCleanup()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.runCleanup()
		}
	}
}

// botIDFromToken extracts the numeric bot id from a BotFather token's
// "<id>:<secret>" prefix -- same format the go-telegram/bot library's own
// Bot.ID() method parses, but available before a *Bot exists (WithDefaultHandler
// needs botUserID at construction time, and it only takes the raw token).
func botIDFromToken(token string) int64 {
	prefix, _, ok := strings.Cut(token, ":")
	if !ok {
		return 0
	}
	id, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func (a *App) runCleanup() {
	cutoff := time.Now().Add(-time.Duration(a.cfg.HistoryTTLDays) * 24 * time.Hour).Unix()
	deleted, err := a.db.DeleteOlderThan(a.ctx, cutoff)
	if err != nil {
		a.logger.Error("history cleanup failed", slog.Any("error", err))
		return
	}
	if deleted > 0 {
		a.logger.Info("history cleanup", slog.Int64("deleted", deleted), slog.Int("ttl_days", a.cfg.HistoryTTLDays))
	}
}
