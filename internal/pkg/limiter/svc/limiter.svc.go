// Package svc: rate limits (sliding window, sqlite-backed) + the money
// budget guard. Both live here because they share one source of truth
// (llm_calls) and one enforcement point: CheckAllowed runs before every LLM
// call, RecordAndMaybePause runs after every one -- SKIP decisions cost
// money too, see the ТЗ's "Считаются LLM-вызовы, а не только отправленные
// ответы".
package svc

import (
	"context"
	"strconv"
	"time"

	"pieceobot/internal/core/cfg"
	"pieceobot/internal/pkg/storage/repo"
)

func startOfDayTS() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
}

func startOfMonthTS() int64 {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Unix()
}

type Service struct {
	calls    *repo.LlmCallsRepo
	settings *repo.SettingsRepo
	cfg      *cfg.Config
}

func New(calls *repo.LlmCallsRepo, settings *repo.SettingsRepo, c *cfg.Config) *Service {
	return &Service{calls: calls, settings: settings, cfg: c}
}

func (s *Service) GetDailyBudget(ctx context.Context) (float64, error) {
	raw, ok, err := s.settings.Get(ctx, "daily_budget_usd")
	if err != nil {
		return 0, err
	}
	if !ok {
		return s.cfg.DailyBudgetUSD, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return s.cfg.DailyBudgetUSD, nil
	}
	return v, nil
}

func (s *Service) SetDailyBudget(ctx context.Context, value float64) error {
	return s.settings.Set(ctx, "daily_budget_usd", strconv.FormatFloat(value, 'f', -1, 64))
}

func (s *Service) IsPaused(ctx context.Context) (bool, error) {
	return s.settings.IsPaused(ctx)
}

func (s *Service) SetPaused(ctx context.Context, paused bool) error {
	return s.settings.SetPaused(ctx, paused)
}

// CheckAllowed runs the cheap pre-flight checks (ТЗ 3.2 step 5 + budget).
func (s *Service) CheckAllowed(ctx context.Context, chatID int64) (bool, string, error) {
	paused, err := s.settings.IsPaused(ctx)
	if err != nil {
		return false, "", err
	}
	if paused {
		return false, "paused", nil
	}

	dailyBudget, err := s.GetDailyBudget(ctx)
	if err != nil {
		return false, "", err
	}
	costToday, err := s.calls.CostSince(ctx, startOfDayTS())
	if err != nil {
		return false, "", err
	}
	if costToday >= dailyBudget {
		_ = s.settings.SetPaused(ctx, true)
		return false, "daily_budget", nil
	}

	costMonth, err := s.calls.CostSince(ctx, startOfMonthTS())
	if err != nil {
		return false, "", err
	}
	if costMonth >= s.cfg.MonthlyBudgetUSD {
		_ = s.settings.SetPaused(ctx, true)
		return false, "monthly_budget", nil
	}

	now := time.Now().Unix()

	globalCount, err := s.calls.CountSince(ctx, now-int64(s.cfg.GlobalWindowSec))
	if err != nil {
		return false, "", err
	}
	if globalCount >= s.cfg.GlobalLimit {
		return false, "global_rate", nil
	}

	chatCount, err := s.calls.CountSinceForChat(ctx, now-int64(s.cfg.ChatWindowSec), chatID)
	if err != nil {
		return false, "", err
	}
	if chatCount >= s.cfg.ChatLimit {
		return false, "chat_rate", nil
	}

	chatDaily, err := s.calls.CountSinceForChat(ctx, now-86400, chatID)
	if err != nil {
		return false, "", err
	}
	if chatDaily >= s.cfg.ChatDailyLimit {
		return false, "chat_daily", nil
	}

	return true, "", nil
}

// RetryDelayForChatDaily returns how long until a "chat_daily" block for
// this chat clears -- the moment its oldest call within the 24h window ages
// out of it, rather than a guessed delay (the window is rolling, not
// calendar-day, so the clear time isn't a fixed offset from now). ok is
// false when there's nothing to wait on (e.g. the block already cleared).
func (s *Service) RetryDelayForChatDaily(ctx context.Context, chatID int64) (delay time.Duration, ok bool, err error) {
	oldest, found, err := s.calls.OldestSinceForChat(ctx, time.Now().Unix()-86400, chatID)
	if err != nil || !found {
		return 0, false, err
	}
	unblockAt := oldest + 86400
	remaining := unblockAt - time.Now().Unix()
	if remaining <= 0 {
		return 0, false, nil
	}
	return time.Duration(remaining+1) * time.Second, true, nil
}

// RecordAndMaybePause records the call and returns true iff this call just
// pushed spend over budget and the bot was not already paused (caller
// should notify the owner).
func (s *Service) RecordAndMaybePause(ctx context.Context, chatID, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64, costUSD float64, result string) (bool, error) {
	if err := s.calls.Record(ctx, chatID, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, costUSD, result); err != nil {
		return false, err
	}

	dailyBudget, err := s.GetDailyBudget(ctx)
	if err != nil {
		return false, err
	}
	costToday, err := s.calls.CostSince(ctx, startOfDayTS())
	if err != nil {
		return false, err
	}
	costMonth, err := s.calls.CostSince(ctx, startOfMonthTS())
	if err != nil {
		return false, err
	}

	if costToday < dailyBudget && costMonth < s.cfg.MonthlyBudgetUSD {
		return false, nil
	}

	wasPaused, err := s.settings.IsPaused(ctx)
	if err != nil {
		return false, err
	}
	if err := s.settings.SetPaused(ctx, true); err != nil {
		return false, err
	}
	return !wasPaused, nil
}

type Stats struct {
	SentToday       int
	CallsLast2Min   int
	TokensInToday   int64
	TokensOutToday  int64
	CostToday       float64
	CostMonth       float64
	DailyBudget     float64
	MonthlyBudget   float64
	BudgetLeftToday float64
}

func (s *Service) GetStats(ctx context.Context) (Stats, error) {
	var out Stats
	var err error

	if out.SentToday, err = s.calls.SentCountSince(ctx, startOfDayTS()); err != nil {
		return out, err
	}
	if out.CallsLast2Min, err = s.calls.CountSince(ctx, time.Now().Unix()-120); err != nil {
		return out, err
	}
	if out.TokensInToday, out.TokensOutToday, err = s.calls.TokensSince(ctx, startOfDayTS()); err != nil {
		return out, err
	}
	if out.CostToday, err = s.calls.CostSince(ctx, startOfDayTS()); err != nil {
		return out, err
	}
	if out.CostMonth, err = s.calls.CostSince(ctx, startOfMonthTS()); err != nil {
		return out, err
	}
	if out.DailyBudget, err = s.GetDailyBudget(ctx); err != nil {
		return out, err
	}
	out.MonthlyBudget = s.cfg.MonthlyBudgetUSD
	out.BudgetLeftToday = out.DailyBudget - out.CostToday
	if out.BudgetLeftToday < 0 {
		out.BudgetLeftToday = 0
	}
	return out, nil
}
