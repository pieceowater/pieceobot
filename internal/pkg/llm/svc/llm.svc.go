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
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"pieceobot/internal/core/cfg"
	personasvc "pieceobot/internal/pkg/persona/svc"
	stickerssvc "pieceobot/internal/pkg/stickers/svc"
	"pieceobot/internal/pkg/storage/repo"
)

const (
	skipMarker      = "SKIP"
	rudeMarker      = "RUDE"
	actionMarker    = "ACTION"
	stickerPrefix   = "STICKER:"
	charsPerToken   = 3 // conservative estimate for mixed Cyrillic/Latin text
	maxMessageChars = 500
)

// RudeNoticeText is sent to the customer (not the model's own words) when
// the model classifies their message as rude/aggressive -- ТЗ follow-up:
// don't just go silent, let them know a human will see it.
const RudeNoticeText = "Ваше сообщение передано владельцу этого автоответчика."

// ActionNoticeText is sent to the customer when their message needs the
// owner's own decision/agreement (money, meetings, promises, "come by",
// invitations, etc.) -- ТЗ follow-up: acknowledge instead of going silent.
const ActionNoticeText = "Я автоответчик — спрошу у владельца, передал инфу."

type Result struct {
	ReplyText           *string
	StickerFileID       *string
	IsRude              bool
	IsAction            bool
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	CostUSD             float64
	Error               bool
}

// IsSkip is true only for a silent, no-reply-sent decision -- a rude-notice
// reply or a sticker reply has ReplyText/StickerFileID set and is not a skip.
func (r Result) IsSkip() bool {
	return !r.Error && r.ReplyText == nil && r.StickerFileID == nil
}

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

func renderLines(messages []repo.Message) []string {
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
	return lines
}

// BuildHistoryText renders messages (oldest first) as compact "Я:"/"Он:"
// lines, split into two labeled sections: background history (trimmed to
// HistoryMaxTokens, oldest dropped first) and the trailing run of customer
// messages that haven't been reacted to yet. The split matters -- without
// it, a model judging the whole blob at once tends to carry a prior rude
// message's tone onto an unrelated new one (observed: "го курить" right
// after "пошел нахуй" got classified RUDE by association). See baseRules'
// instruction to judge only the "Новое сообщение" section.
func BuildHistoryText(messages []repo.Message, c *cfg.Config) string {
	if len(messages) == 0 {
		return ""
	}

	splitIdx := len(messages)
	for splitIdx > 0 && !messages[splitIdx-1].FromMe {
		splitIdx--
	}
	background, newTurn := messages[:splitIdx], messages[splitIdx:]
	if len(newTurn) == 0 {
		// Defensive only -- processBatch always calls this right after
		// storing a genuine new customer message, so this shouldn't happen.
		background, newTurn = nil, messages
	}

	bgLines := renderLines(background)
	newLines := renderLines(newTurn)

	maxChars := c.HistoryMaxTokens * charsPerToken
	for len(bgLines) > 0 && totalLen(bgLines)+totalLen(newLines) > maxChars {
		bgLines = bgLines[1:]
	}

	var sb strings.Builder
	if len(bgLines) > 0 {
		sb.WriteString("История (только для тона и общего фона):\n")
		sb.WriteString(strings.Join(bgLines, "\n"))
		sb.WriteString("\n\n")
	}
	sb.WriteString("Новое сообщение — отреагируй именно на него:\n")
	sb.WriteString(strings.Join(newLines, "\n"))
	return sb.String()
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
	stickers     map[string]string
	logger       *slog.Logger
}

func New(c *cfg.Config, logger *slog.Logger) (*Service, error) {
	personaText, err := personasvc.LoadPersona(c.PersonaPath)
	if err != nil {
		return nil, err
	}
	stickers, err := stickerssvc.Load(c.StickersPath)
	if err != nil {
		return nil, err
	}
	client := anthropic.NewClient(
		option.WithAPIKey(c.AnthropicAPIKey),
		option.WithMaxRetries(1), // one retry with backoff, then give up -- ТЗ 6
	)
	systemPrompt := personasvc.BuildSystemPrompt(personaText) + stickerInstructions(stickers)
	return &Service{
		cfg:          c,
		client:       client,
		systemPrompt: systemPrompt,
		stickers:     stickers,
		logger:       logger,
	}, nil
}

// FallbackStickerFileID picks a sticker to use as a guaranteed reply when a
// bare incoming sticker must not go silent (ТЗ follow-up) but the model
// decided to skip anyway. Prefers a tag named "ok", else the first tag in
// sorted order, for a deterministic, reproducible choice.
func (s *Service) FallbackStickerFileID() (string, bool) {
	if len(s.stickers) == 0 {
		return "", false
	}
	if fileID, ok := s.stickers["ok"]; ok {
		return fileID, true
	}
	tags := make([]string, 0, len(s.stickers))
	for tag := range s.stickers {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return s.stickers[tags[0]], true
}

// stickerInstructions documents the available sticker tags in the prompt --
// omitted entirely when there are none, so an unconfigured bot never sees
// STICKER: mentioned at all.
func stickerInstructions(stickers map[string]string) string {
	if len(stickers) == 0 {
		return ""
	}
	tags := make([]string, 0, len(stickers))
	for tag := range stickers {
		tags = append(tags, tag)
	}
	sort.Strings(tags) // deterministic prompt text
	return "\n\nЕсли уместнее ответить стикером, а не текстом, и он точно подходит по смыслу/настроению " +
		"(не используй просто так) — вместо текста выведи ровно STICKER:<тег> и больше ничего. " +
		"Если собеседник сам прислал стикер или гифку (в истории это выглядит как \"[стикер]\"/\"[gif]\") — " +
		"это нормальный повод ответить в тон подходящим стикером, если такой есть среди тегов; " +
		"если точного совпадения по смыслу нет — просто ответь текстом или SKIP, не подбирай стикер наугад. " +
		"Доступные теги: " + strings.Join(tags, ", ") + "."
}

// parseModelOutput interprets the model's raw text: SKIP -> silent (all nil),
// RUDE/ACTION -> their canned customer-facing notices, STICKER:<tag> -> a
// resolved file_id (or a silent skip if the tag is unknown/hallucinated),
// anything else -> a normal reply. Pure function, no I/O -- see llm_test.go.
func parseModelOutput(rawText string, stickers map[string]string) (replyText, stickerFileID *string, isRude, isAction bool) {
	text := strings.TrimSpace(rawText)
	switch {
	case text == "" || strings.EqualFold(text, skipMarker):
		// silent skip -- everything stays nil
	case strings.EqualFold(text, rudeMarker):
		isRude = true
		notice := RudeNoticeText
		replyText = &notice
	case strings.EqualFold(text, actionMarker):
		isAction = true
		notice := ActionNoticeText
		replyText = &notice
	case len(text) > len(stickerPrefix) && strings.EqualFold(text[:len(stickerPrefix)], stickerPrefix):
		tag := strings.TrimSpace(text[len(stickerPrefix):])
		if fileID, ok := stickers[tag]; ok {
			stickerFileID = &fileID
		}
		// unknown/hallucinated tag -- falls through as a silent skip
	default:
		replyText = &text
	}
	return replyText, stickerFileID, isRude, isAction
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

	replyText, stickerFileID, isRude, isAction := parseModelOutput(text, s.stickers)

	return Result{
		ReplyText:           replyText,
		StickerFileID:       stickerFileID,
		IsRude:              isRude,
		IsAction:            isAction,
		InputTokens:         msg.Usage.InputTokens,
		OutputTokens:        msg.Usage.OutputTokens,
		CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadTokens:     msg.Usage.CacheReadInputTokens,
		CostUSD:             ComputeCost(msg.Usage, s.cfg),
	}
}
