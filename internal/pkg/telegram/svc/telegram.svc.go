// Package svc: the Business-update pipeline (ТЗ 3.2's filter chain, in
// order, each step able to stop before any LLM call happens) plus the
// owner-only control commands (commands.go, same package). One Service is
// registered as the bot's single default handler (see internal/app.go) and
// dispatches on which Update field is set.
package svc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"pieceobot/internal/core/cfg"
	debouncesvc "pieceobot/internal/pkg/debounce/svc"
	limitersvc "pieceobot/internal/pkg/limiter/svc"
	llmsvc "pieceobot/internal/pkg/llm/svc"
	"pieceobot/internal/pkg/storage/repo"
)

const processTimeout = 60 * time.Second

// fallbackEmoji is the last-resort reply to a bare sticker when no
// stickers.md tag matches -- picked from the owner's own emoji whitelist
// (persona.md), not a generic smiley.
const fallbackEmoji = "🔥"

// A chat with fewer than this many of the owner's own messages hasn't
// established enough tone of its own -- pull in cross-chat style examples
// (see llmsvc.PrependStyleExamples) rather than relying only on persona.md.
const minChatHistoryForOwnStyle = 3

// styleExampleCount is how many of the owner's genuinely hand-typed
// messages (across all chats) to offer as style exemplars.
const styleExampleCount = 10

type Service struct {
	cfg          *cfg.Config
	logger       *slog.Logger
	messages     *repo.MessagesRepo
	chatState    *repo.ChatStateRepo
	businessConn *repo.BusinessConnectionsRepo
	limiter      *limitersvc.Service
	debounce     *debouncesvc.Service
	llm          *llmsvc.Service
	botUserID    int64
}

func New(
	c *cfg.Config,
	logger *slog.Logger,
	messages *repo.MessagesRepo,
	chatState *repo.ChatStateRepo,
	businessConn *repo.BusinessConnectionsRepo,
	limiter *limitersvc.Service,
	debounce *debouncesvc.Service,
	llm *llmsvc.Service,
	botUserID int64,
) *Service {
	return &Service{
		cfg: c, logger: logger, messages: messages, chatState: chatState,
		businessConn: businessConn, limiter: limiter, debounce: debounce,
		llm: llm, botUserID: botUserID,
	}
}

// CatchUpPending resumes chats left unanswered across a restart -- the
// debounce timer that would have fired their reply lives only in the
// previous process's memory and dies with it. Called once from app.go's
// Start(), before/alongside polling. Each pending chat still goes through
// the normal shouldConsider() guards (mute, owner-active, rate limits) via
// processBatch, so this can't bypass any of them.
func (s *Service) CatchUpPending(ctx context.Context, b *tgbot.Bot) {
	connID, ok, err := s.businessConn.OwnerConnectionID(ctx)
	if err != nil {
		s.logger.Error("catch-up: failed to look up owner connection", slog.Any("error", err))
		return
	}
	if !ok {
		return // no active owner connection yet -- nothing to resume
	}

	chatIDs, err := s.chatState.PendingChatIDs(ctx)
	if err != nil {
		s.logger.Error("catch-up: failed to list pending chats", slog.Any("error", err))
		return
	}
	for _, chatID := range chatIDs {
		s.logger.Info("catching up on a chat left unanswered before restart", slog.Int64("chat_id", chatID))
		s.processBatch(ctx, b, chatID, connID)
	}
}

// HandleUpdate is registered via bot.WithDefaultHandler -- ТЗ 2's four
// business update types plus plain messages (owner commands), explicitly
// requested via bot.WithAllowedUpdates in app.go.
func (s *Service) HandleUpdate(ctx context.Context, b *tgbot.Bot, update *models.Update) {
	switch {
	case update.BusinessConnection != nil:
		s.handleBusinessConnection(ctx, update.BusinessConnection)
	case update.BusinessMessage != nil:
		s.handleIncoming(ctx, b, update.BusinessMessage)
	case update.EditedBusinessMessage != nil:
		s.logger.Debug("edited_business_message ignored", slog.Int64("chat_id", update.EditedBusinessMessage.Chat.ID))
	case update.DeletedBusinessMessages != nil:
		s.logger.Debug("deleted_business_messages",
			slog.Int64("chat_id", update.DeletedBusinessMessages.Chat.ID),
			slog.Int("count", len(update.DeletedBusinessMessages.MessageIDs)))
	case update.Message != nil:
		s.handleCommand(ctx, b, update.Message)
	}
}

func (s *Service) storeConnection(ctx context.Context, bc *models.BusinessConnection) repo.BusinessConnRow {
	isOwner := bc.User.ID == s.cfg.OwnerUserID
	canReply := false
	if bc.Rights != nil {
		canReply = bc.Rights.CanReply
	}
	if err := s.businessConn.Upsert(ctx, bc.ID, bc.User.ID, isOwner, bc.IsEnabled, canReply); err != nil {
		s.logger.Error("failed to store business_connection", slog.Any("error", err))
	}
	return repo.BusinessConnRow{OwnerUserID: bc.User.ID, IsOwner: isOwner, IsEnabled: bc.IsEnabled, CanReply: canReply}
}

func (s *Service) handleBusinessConnection(ctx context.Context, bc *models.BusinessConnection) {
	row := s.storeConnection(ctx, bc)
	if !row.IsOwner {
		// Someone other than the account this bot is meant for connected it --
		// never reply there. See the is_owner check in handleIncoming below.
		s.logger.Warn("business_connection from non-owner user -- will be ignored", slog.Int64("user_id", bc.User.ID))
		return
	}
	s.logger.Info("business_connection", slog.String("id", bc.ID), slog.Bool("enabled", bc.IsEnabled), slog.Bool("can_reply", row.CanReply))
}

func extractText(msg *models.Message) (string, bool) {
	if msg.Text != "" {
		return msg.Text, true
	}
	switch {
	case msg.Sticker != nil:
		return "[стикер]", true
	case len(msg.Photo) > 0:
		return "[фото]", true
	case msg.Voice != nil:
		return "[голосовое]", true
	case msg.Video != nil:
		return "[видео]", true
	case msg.VideoNote != nil:
		return "[видеосообщение]", true
	case msg.Document != nil:
		return "[файл]", true
	case msg.Animation != nil:
		return "[gif]", true
	case msg.Audio != nil:
		return "[аудио]", true
	case msg.Location != nil:
		return "[локация]", true
	case msg.Contact != nil:
		return "[контакт]", true
	case msg.Poll != nil:
		return "[опрос]", true
	}
	return "", false
}

// displayName builds a human-readable name for owner notifications --
// first+last name, falling back to @username on the rare account with no
// first name set.
func displayName(u *models.User) string {
	name := strings.TrimSpace(u.FirstName)
	if u.LastName != "" {
		name = strings.TrimSpace(name + " " + u.LastName)
	}
	if name == "" && u.Username != "" {
		name = "@" + u.Username
	}
	return name
}

func (s *Service) handleIncoming(ctx context.Context, b *tgbot.Bot, msg *models.Message) {
	connID := msg.BusinessConnectionID
	if connID == "" || msg.Chat.Type != models.ChatTypePrivate || msg.From == nil {
		return
	}

	owner := s.cfg.OwnerUserID
	chatID := msg.Chat.ID
	isFromOwnerSide := msg.From.ID == owner
	sentByThisBot := msg.SenderBusinessBot != nil && msg.SenderBusinessBot.ID == s.botUserID
	text, hasText := extractText(msg)

	if isFromOwnerSide {
		if err := s.chatState.TouchOutgoing(ctx, chatID); err != nil {
			s.logger.Error("touch_outgoing failed", slog.Any("error", err))
		}
		if !sentByThisBot {
			// Real human typed this from their own device -- ТЗ 3.2 step 3:
			// the bot stays quiet on this chat for OwnerActivePauseMin.
			if err := s.chatState.TouchOwnerManualMessage(ctx, chatID); err != nil {
				s.logger.Error("touch_owner_manual failed", slog.Any("error", err))
			}
		}
		if hasText {
			_ = s.messages.Add(ctx, chatID, true, !sentByThisBot, text)
		}
		return
	}

	if msg.From.IsBot {
		return // ignore other bots entirely -- ТЗ 3.1
	}

	connRow, err := s.businessConn.Get(ctx, connID)
	if err != nil {
		s.logger.Error("failed to load business_connection", slog.Any("error", err))
		return
	}
	if connRow == nil {
		// First message we've seen for this connection (e.g. right after a
		// restart) -- fetch it directly instead of waiting for an update.
		bc, err := b.GetBusinessConnection(ctx, &tgbot.GetBusinessConnectionParams{BusinessConnectionID: connID})
		if err != nil {
			s.logger.Error("failed to fetch business_connection", slog.Any("error", err))
			return
		}
		row := s.storeConnection(ctx, bc)
		connRow = &row
	}

	if !connRow.IsOwner {
		return // never operate on a connection that isn't this bot's owner
	}
	if !connRow.IsEnabled || !connRow.CanReply {
		s.logger.Info("business_connection disabled or lacks reply rights -- ignored",
			slog.String("conn_id", connID), slog.Int64("chat_id", chatID))
		return
	}

	if err := s.chatState.TouchIncoming(ctx, chatID); err != nil {
		s.logger.Error("touch_incoming failed", slog.Any("error", err))
	}
	if err := s.chatState.SetDisplayName(ctx, chatID, displayName(msg.From)); err != nil {
		s.logger.Error("set_display_name failed", slog.Any("error", err))
	}
	if hasText {
		_ = s.messages.Add(ctx, chatID, false, false, text)
	}
	if !hasText {
		return // media placeholder stored, never replied to
	}

	s.debounce.Trigger(chatID, func() {
		bg, cancel := context.WithTimeout(context.Background(), processTimeout)
		defer cancel()
		s.processBatch(bg, b, chatID, connID)
	})
}

// shouldConsider also returns the blocking reason (mirrors
// limiter.CheckAllowed's own reason) so processBatch can react to specific
// ones -- e.g. "chat_rate" gets an automatic retry once the window clears,
// see processBatch's use of debounce.TriggerAfter.
func (s *Service) shouldConsider(ctx context.Context, chatID int64) (bool, string) {
	paused, err := s.limiter.IsPaused(ctx)
	if err != nil {
		s.logger.Error("is_paused check failed", slog.Any("error", err))
		return false, "error"
	}
	if paused {
		return false, "paused"
	}
	if _, blocked := s.cfg.Blacklist[chatID]; blocked {
		return false, "blacklist"
	}
	if len(s.cfg.Whitelist) > 0 {
		if _, allowed := s.cfg.Whitelist[chatID]; !allowed {
			return false, "whitelist"
		}
	}

	state, err := s.chatState.Get(ctx, chatID)
	if err != nil {
		s.logger.Error("chat_state get failed", slog.Any("error", err))
		return false, "error"
	}
	if state.Muted {
		return false, "muted"
	}
	if state.LastOwnerMsgTS != nil {
		elapsedMin := float64(time.Now().Unix()-*state.LastOwnerMsgTS) / 60
		if elapsedMin < float64(s.cfg.OwnerActivePauseMin) {
			return false, "owner_active"
		}
	}
	if state.LastMessageFromMe {
		return false, "already_answered"
	}

	allowed, reason, err := s.limiter.CheckAllowed(ctx, chatID)
	if err != nil {
		s.logger.Error("limiter check failed", slog.Any("error", err))
		return false, "error"
	}
	return allowed, reason
}

func (s *Service) processBatch(ctx context.Context, b *tgbot.Bot, chatID int64, connID string) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("processBatch panicked", slog.Any("panic", r))
		}
	}()

	if allowed, reason := s.shouldConsider(ctx, chatID); !allowed {
		if reason == "chat_rate" {
			// Stay silent to the customer (no "I'm rate limited" notice --
			// it reads oddly and the retry below makes it unnecessary: don't
			// just go silent until another message happens to arrive, retry
			// once the window that's blocking us has rolled over, and reply
			// to everything that piled up in the meantime as one batch.
			retryDelay := time.Duration(s.cfg.ChatWindowSec) * time.Second
			s.debounce.TriggerAfter(chatID, retryDelay, func() {
				bg, cancel := context.WithTimeout(context.Background(), processTimeout)
				defer cancel()
				s.processBatch(bg, b, chatID, connID)
			})
		}
		return
	}

	history, err := s.messages.Recent(ctx, chatID, s.cfg.HistoryMessages)
	if err != nil {
		s.logger.Error("failed to load history", slog.Any("error", err))
		return
	}
	historyText := llmsvc.BuildHistoryText(history, s.cfg)
	if historyText == "" {
		return
	}

	ownLinesInChat := 0
	for _, m := range history {
		if m.FromMe {
			ownLinesInChat++
		}
	}
	if ownLinesInChat < minChatHistoryForOwnStyle {
		// New or barely-talked-to contact -- give the model real examples
		// of the owner's voice from elsewhere instead of relying solely on
		// persona.md's description.
		if examples, err := s.messages.RecentManualSamples(ctx, styleExampleCount); err != nil {
			s.logger.Error("failed to load style examples", slog.Any("error", err))
		} else if len(examples) > 0 {
			historyText = llmsvc.PrependStyleExamples(historyText, examples)
		}
	}

	latestIncoming := latestIncomingText(history)
	state, err := s.chatState.Get(ctx, chatID)
	if err != nil {
		s.logger.Error("failed to load chat_state for notification", slog.Any("error", err))
	}
	contact := contactLabel(state.DisplayName, chatID)

	result := s.llm.DecideReply(ctx, historyText)

	// A bare sticker must never go silent (ТЗ follow-up) -- if the model
	// skipped it anyway, force a reply: a configured sticker if we have
	// one, else a plain emoji acknowledgement (from persona.md's own
	// whitelist, not a generic smiley -- see fallbackEmoji).
	if result.IsSkip() && isStickerOnlyBatch(latestIncoming) {
		if fileID, ok := s.llm.FallbackStickerFileID(); ok {
			result.StickerFileID = &fileID
		} else {
			fallback := fallbackEmoji
			result.ReplyText = &fallback
		}
	}

	resultLabel := "skip"
	switch {
	case result.Error:
		resultLabel = "error"
	case result.StickerFileID != nil:
		resultLabel = "sticker"
	case result.IsRude:
		resultLabel = "rude"
	case result.IsAction:
		resultLabel = "action"
	case result.ReplyText != nil:
		resultLabel = "sent"
	}
	justPaused, err := s.limiter.RecordAndMaybePause(ctx, chatID,
		result.InputTokens, result.OutputTokens, result.CacheCreationTokens, result.CacheReadTokens,
		result.CostUSD, resultLabel)
	if err != nil {
		s.logger.Error("failed to record llm call", slog.Any("error", err))
	}
	if justPaused {
		s.notifyOwner(ctx, b, "Бюджет исчерпан — бот встал на паузу.")
	}

	if result.Error || (result.ReplyText == nil && result.StickerFileID == nil) {
		if result.IsSkip() && s.cfg.NotifyOnSkip {
			s.notifyOwner(ctx, b, fmt.Sprintf("Пропустил (%s):\n%s", contact, latestIncoming))
		}
		return
	}

	if result.StickerFileID != nil {
		if _, err := b.SendSticker(ctx, &tgbot.SendStickerParams{
			ChatID:               chatID,
			Sticker:              &models.InputFileString{Data: *result.StickerFileID},
			BusinessConnectionID: connID,
		}); err != nil {
			s.logger.Error("failed to send sticker", slog.Any("error", err))
			return
		}
		_ = s.messages.Add(ctx, chatID, true, false, "[стикер]")
	} else {
		if _, err := b.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID:               chatID,
			Text:                 *result.ReplyText,
			BusinessConnectionID: connID,
		}); err != nil {
			s.logger.Error("failed to send message", slog.Any("error", err))
			return
		}
		_ = s.messages.Add(ctx, chatID, true, false, *result.ReplyText)
	}

	if err := s.chatState.TouchOutgoing(ctx, chatID); err != nil {
		s.logger.Error("touch_outgoing failed", slog.Any("error", err))
	}

	switch {
	case result.IsRude:
		// Always tell the owner about rude contacts, independent of
		// NotifyOnSkip -- this isn't a skip, a reply was actually sent.
		s.notifyOwner(ctx, b, fmt.Sprintf("Грубость (%s):\n%s", contact, latestIncoming))
	case result.IsAction:
		// Same reasoning -- the customer was told "передал инфу", so the
		// owner actually needs to see it now, not just on NotifyOnSkip.
		s.notifyOwner(ctx, b, fmt.Sprintf("Нужно решение (%s):\n%s", contact, latestIncoming))
	}
}

// contactLabel prefers the customer's captured name -- a plain "id N" isn't
// clickable/useful on its own, and neither a raw tg:// link nor an inline
// "open chat" button render reliably across Telegram clients (the button
// can even fail the whole send outright on some accounts' privacy settings,
// BUTTON_USER_PRIVACY_RESTRICTED) -- so this drops both in favor of a name.
// The numeric id stays in parens purely so /mute and /unmute can still
// parse a target from a replied-to notification.
func contactLabel(name string, chatID int64) string {
	if name == "" {
		return fmt.Sprintf("id %d", chatID)
	}
	return fmt.Sprintf("%s, id %d", name, chatID)
}

func (s *Service) notifyOwner(ctx context.Context, b *tgbot.Bot, text string) {
	if _, err := b.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: s.cfg.OwnerUserID, Text: text}); err != nil {
		s.logger.Error("failed to notify owner", slog.Any("error", err))
	}
}

// latestIncomingText returns the customer's trailing run of messages since
// this bot/the owner last spoke in this chat -- the exact, untruncated
// content a skip/rude notification should show (ТЗ follow-up: forward the
// real message, not an 80-char clipped preview).
func latestIncomingText(history []repo.Message) string {
	var lines []string
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].FromMe {
			break
		}
		lines = append([]string{history[i].Text}, lines...)
	}
	return strings.Join(lines, "\n")
}

// isStickerOnlyBatch reports whether the latest incoming turn is nothing
// but sticker placeholders -- the case that must never be silently skipped.
func isStickerOnlyBatch(latestIncoming string) bool {
	if latestIncoming == "" {
		return false
	}
	for _, line := range strings.Split(latestIncoming, "\n") {
		if line != "[стикер]" {
			return false
		}
	}
	return true
}
