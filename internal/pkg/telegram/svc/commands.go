package svc

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Owner-only control commands, sent directly to the bot (not via Business).
// Everything here is gated on OwnerUserID -- anyone else's commands are
// silently ignored (ТЗ 3.5).

var chatIDInText = regexp.MustCompile(`\(id:\s*(-?\d+)\)`)

func (s *Service) handleCommand(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	if msg.Chat.Type != models.ChatTypePrivate || msg.From == nil || msg.From.ID != s.cfg.OwnerUserID {
		return
	}

	if msg.Sticker != nil {
		// Owner sent a sticker directly to the bot -- surface its file_id so
		// they can add it to STICKERS_PATH (ТЗ follow-up: sticker replies).
		s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf(
			"file_id: %s\n\nДобавь строку в %s: <тег>: %s",
			msg.Sticker.FileID, s.cfg.StickersPath, msg.Sticker.FileID,
		))
		return
	}

	if !strings.HasPrefix(msg.Text, "/") {
		return
	}

	fields := strings.SplitN(msg.Text, " ", 2)
	cmd := strings.SplitN(fields[0], "@", 2)[0] // strip a "/cmd@botname" suffix
	var arg string
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}

	switch cmd {
	case "/pause":
		s.cmdPause(ctx, b, msg)
	case "/resume":
		s.cmdResume(ctx, b, msg)
	case "/mute":
		s.cmdMute(ctx, b, msg, arg)
	case "/unmute":
		s.cmdUnmute(ctx, b, msg, arg)
	case "/budget":
		s.cmdBudget(ctx, b, msg, arg)
	case "/stats":
		s.cmdStats(ctx, b, msg)
	}
}

func (s *Service) reply(ctx context.Context, b *tgbot.Bot, chatID int64, text string) {
	if _, err := b.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text}); err != nil {
		s.logger.Error("failed to reply to owner command", slog.Any("error", err))
	}
}

func targetChatID(msg *models.Message, arg string) (int64, bool) {
	if arg != "" {
		if v, err := strconv.ParseInt(arg, 10, 64); err == nil {
			return v, true
		}
	}
	if msg.ReplyToMessage != nil && msg.ReplyToMessage.Text != "" {
		if m := chatIDInText.FindStringSubmatch(msg.ReplyToMessage.Text); m != nil {
			if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

func (s *Service) cmdPause(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	_ = s.limiter.SetPaused(ctx, true)
	s.reply(ctx, b, msg.Chat.ID, "Бот на паузе.")
}

func (s *Service) cmdResume(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	_ = s.limiter.SetPaused(ctx, false)
	s.reply(ctx, b, msg.Chat.ID, "Бот снова отвечает.")
}

func (s *Service) cmdMute(ctx context.Context, b *tgbot.Bot, msg *models.Message, arg string) {
	chatID, ok := targetChatID(msg, arg)
	if !ok {
		s.reply(ctx, b, msg.Chat.ID, "Укажи /mute <user_id> или ответь на уведомление о чате.")
		return
	}
	_ = s.chatState.SetMuted(ctx, chatID, true)
	s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf("Чат %d замьючен.", chatID))
}

func (s *Service) cmdUnmute(ctx context.Context, b *tgbot.Bot, msg *models.Message, arg string) {
	chatID, ok := targetChatID(msg, arg)
	if !ok {
		s.reply(ctx, b, msg.Chat.ID, "Укажи /unmute <user_id> или ответь на уведомление о чате.")
		return
	}
	_ = s.chatState.SetMuted(ctx, chatID, false)
	s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf("Чат %d размьючен.", chatID))
}

func (s *Service) cmdBudget(ctx context.Context, b *tgbot.Bot, msg *models.Message, arg string) {
	if arg == "" {
		current, _ := s.limiter.GetDailyBudget(ctx)
		s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf("Текущий дневной бюджет: $%.2f. Использование: /budget <usd>", current))
		return
	}
	value, err := strconv.ParseFloat(arg, 64)
	if err != nil {
		s.reply(ctx, b, msg.Chat.ID, "Не понял число. Пример: /budget 2.5")
		return
	}
	_ = s.limiter.SetDailyBudget(ctx, value)
	s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf("Дневной бюджет: $%.2f", value))
}

func (s *Service) cmdStats(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	stats, err := s.limiter.GetStats(ctx)
	if err != nil {
		s.reply(ctx, b, msg.Chat.ID, "Не смог посчитать статистику.")
		return
	}
	paused, _ := s.limiter.IsPaused(ctx)
	pausedText := "нет"
	if paused {
		pausedText = "да"
	}
	s.reply(ctx, b, msg.Chat.ID, fmt.Sprintf(
		"Статистика:\nОтветов сегодня: %d\nLLM-вызовов за 2 мин: %d\nТокены сегодня: %d in / %d out\n"+
			"Расход сегодня: $%.4f (лимит $%.2f)\nРасход за месяц: $%.4f (лимит $%.2f)\n"+
			"Остаток дневного бюджета: $%.4f\nПауза: %s",
		stats.SentToday, stats.CallsLast2Min, stats.TokensInToday, stats.TokensOutToday,
		stats.CostToday, stats.DailyBudget, stats.CostMonth, stats.MonthlyBudget,
		stats.BudgetLeftToday, pausedText,
	))
}
