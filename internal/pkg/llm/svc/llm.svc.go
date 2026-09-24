// Package svc: the one LLM call -- decide whether to reply, and with what.
// Single call, no classify-then-answer two-step (ТЗ 3.3/4.4): the model
// returns either a short reply or the literal marker SKIP.
//
// No prompt caching: the system prompt (fixed rules + persona.md) is on the
// order of a few hundred tokens, far under Haiku 4.5's 4096-token minimum
// cacheable prefix -- a cache_control marker here would pay the write
// premium and never get a read. Revisit only if persona.md grows past
// ~3000 tokens.
package svc

import (
	"context"
	"log/slog"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"pieceobot/internal/core/cfg"
	"pieceobot/internal/pkg/persona/svc"
	"pieceobot/internal/pkg/storage/repo"
)

const (
	skipMarker      = "SKIP"
	charsPerToken   = 3 // conservative estimate for mixed Cyrillic/Latin text
	maxMessageChars = 500
)

type Result struct {
	ReplyText           *string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	CostUSD             float64
	Error               bool
}

func (r Result) IsSkip() bool { return !r.Error && r.ReplyText == nil }

func ComputeCost(u anthropic.Usage, c *cfg.Config) float64 {
	priceIn := c.PriceInputPerMTok / 1_000_000
	priceOut := c.PriceOutputPerMTok / 1_000_000
	priceCacheWrite := priceIn * 1.25 // 5-minute TTL premium
	priceCacheRead := priceIn * 0.1
	return float64(u.InputTokens)*priceIn +
		float64(u.OutputTokens)*priceOut +
		float64(u.CacheCreationInputTokens)*priceCacheWrite +
		float64(u.CacheReadInputTokens)*priceCacheRead
}

// BuildHistoryText renders messages (oldest first) as compact "Я:"/"Он:"
// lines, trimmed to HistoryMaxTokens (rough char-based estimate).
func BuildHistoryText(messages []repo.Message, c *cfg.Config) string {
	lines := make([]string, 0, len(messages))
	for _, m := range messages {
		text := strings.ReplaceAll(m.Text, "\n", " ")
		text = strings.TrimSpace(text)
		if len([]rune(text)) > maxMessageChars {
			text = string([]rune(text)[:maxMessageChars]) + "…"
		}
		prefix := "Он"
		if m.FromMe {
			prefix = "Я"
		}
		lines = append(lines, prefix+": "+text)
	}

	maxChars := c.HistoryMaxTokens * charsPerToken
	for len(lines) > 0 && totalLen(lines) > maxChars {
		lines = lines[1:]
	}
	return strings.Join(lines, "\n")
}

func totalLen(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l)
	}
	return n
}

type Service struct {
	cfg          *cfg.Config
	client       anthropic.Client
	systemPrompt string
	logger       *slog.Logger
}

func New(c *cfg.Config, logger *slog.Logger) (*Service, error) {
	personaText, err := svc.LoadPersona(c.PersonaPath)
	if err != nil {
		return nil, err
	}
	client := anthropic.NewClient(
		option.WithAPIKey(c.AnthropicAPIKey),
		option.WithMaxRetries(1), // one retry with backoff, then give up -- ТЗ 6
	)
	return &Service{
		cfg:          c,
		client:       client,
		systemPrompt: svc.BuildSystemPrompt(personaText),
		logger:       logger,
	}, nil
}

func (s *Service) DecideReply(ctx context.Context, historyText string) Result {
	msg, err := s.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:       s.cfg.LLMModel,
		MaxTokens:   s.cfg.MaxOutputTokens,
		Temperature: anthropic.Float(0.7),
		System:      []anthropic.TextBlockParam{{Text: s.systemPrompt}},
		Messages:    []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(historyText))},
	})
	if err != nil {
		s.logger.Warn("llm call failed", slog.Any("error", err))
		return Result{Error: true}
	}

	var text string
	for _, block := range msg.Content {
		if block.Type == "text" {
			text = block.Text
			break
		}
	}
	text = strings.TrimSpace(text)

	var replyText *string
	if text != "" && !strings.EqualFold(text, skipMarker) {
		replyText = &text
	}

	return Result{
		ReplyText:           replyText,
		InputTokens:         msg.Usage.InputTokens,
		OutputTokens:        msg.Usage.OutputTokens,
		CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadTokens:     msg.Usage.CacheReadInputTokens,
		CostUSD:             ComputeCost(msg.Usage, s.cfg),
	}
}
