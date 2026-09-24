package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
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

// mockTelegramServer answers getMe/sendMessage/getBusinessConnection like
// the real Bot API, and records every sendMessage call for assertions.
type mockTelegramServer struct {
	mu   sync.Mutex
	sent []sentMsg
}

type sentMsg struct {
	ChatID float64
	Text   string
	ConnID string
}

func newMockServer(t *testing.T, botID int64) (*httptest.Server, *mockTelegramServer) {
	m := &mockTelegramServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"id": botID, "is_bot": true, "first_name": "test", "username": "testbot"},
			})
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			_ = r.ParseMultipartForm(1 << 20)
			chatID := 0.0
			fmt.Sscanf(r.FormValue("chat_id"), "%f", &chatID)
			text := r.FormValue("text")
			connID := r.FormValue("business_connection_id")
			m.mu.Lock()
			m.sent = append(m.sent, sentMsg{ChatID: chatID, Text: text, ConnID: connID})
			m.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "result": map[string]any{"message_id": 1, "date": time.Now().Unix(), "chat": map[string]any{"id": chatID, "type": "private"}},
			})
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, m
}

func (m *mockTelegramServer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

func (m *mockTelegramServer) snapshot() []sentMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sentMsg, len(m.sent))
	copy(out, m.sent)
	return out
}

func (m *mockTelegramServer) last() sentMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent[len(m.sent)-1]
}

func TestBusinessPipelineOwnershipGuardAndDebounce(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set, skipping live LLM integration test")
	}

	const ownerID = int64(1000)
	const strangerID = int64(2000)
	const botToken = "999:AAHtestdummydummydummydummydummydummy"

	os.Setenv("BOT_TOKEN", botToken)
	os.Setenv("ANTHROPIC_API_KEY", apiKey)
	os.Setenv("OWNER_USER_ID", "1000")
	os.Setenv("DEBOUNCE_SEC", "1")
	os.Setenv("DEBOUNCE_MAX_SEC", "2")
	os.Setenv("DB_PATH", ":memory:")
	os.Setenv("PERSONA_PATH", "/nonexistent-persona.md")
	c := cfg.Inst()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	sqlDB, err := repo.Connect(":memory:")
	if err != nil {
		t.Fatalf("connect db: %v", err)
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
		t.Fatalf("llm.New: %v", err)
	}

	svc := telegramsvc.New(c, logger, messagesRepo, chatStateRepo, businessConnRepo, limiter, debounce, llm, 999)

	srv, mock := newMockServer(t, 999)
	b, err := tgbot.New(botToken, tgbot.WithServerURL(srv.URL))
	if err != nil {
		t.Fatalf("tgbot.New: %v", err)
	}

	now := time.Now()
	ctx := context.Background()

	// --- Scenario A: connection from the actual owner -> should reply ---
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessConnection: &models.BusinessConnection{
			ID: "connA", User: models.User{ID: ownerID}, UserChatID: ownerID, Date: now.Unix(),
			IsEnabled: true, Rights: &models.BusinessBotRights{CanReply: true},
		},
	})
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 1, Date: int(now.Unix()), Chat: models.Chat{ID: 3000, Type: models.ChatTypePrivate},
			From: &models.User{ID: 3000}, Text: "Привет, как дела?", BusinessConnectionID: "connA",
		},
	})

	deadline := time.Now().Add(6 * time.Second)
	for mock.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if mock.count() != 1 {
		t.Fatalf("scenario A: expected 1 sent message, got %d", mock.count())
	}
	last := mock.last()
	if last.ConnID != "connA" || last.Text == "" {
		t.Fatalf("scenario A: unexpected sent message: %+v", last)
	}
	t.Logf("scenario A reply: %q", last.Text)

	// --- Scenario B: connection from a STRANGER -> must NEVER reply ---
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessConnection: &models.BusinessConnection{
			ID: "connB", User: models.User{ID: strangerID}, UserChatID: strangerID, Date: now.Unix(),
			IsEnabled: true, Rights: &models.BusinessBotRights{CanReply: true},
		},
	})
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 2, Date: int(now.Unix()), Chat: models.Chat{ID: 3001, Type: models.ChatTypePrivate},
			From: &models.User{ID: 3001}, Text: "Привет, как дела?", BusinessConnectionID: "connB",
		},
	})
	time.Sleep(3 * time.Second)
	if mock.count() != 1 {
		t.Fatalf("scenario B: stranger connection must never trigger a reply, got %d total sends", mock.count())
	}
	t.Log("scenario B: correctly ignored stranger's business connection")

	// --- Scenario C: burst of 3 messages -> exactly one more reply ---
	for i, txt := range []string{"Привет", "ты тут?", "как сам вообще"} {
		svc.HandleUpdate(ctx, b, &models.Update{
			BusinessMessage: &models.Message{
				ID: 10 + i, Date: int(now.Unix()), Chat: models.Chat{ID: 3002, Type: models.ChatTypePrivate},
				From: &models.User{ID: 3002}, Text: txt, BusinessConnectionID: "connA",
			},
		})
		time.Sleep(300 * time.Millisecond)
	}
	deadline = time.Now().Add(6 * time.Second)
	for mock.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if mock.count() != 2 {
		t.Fatalf("scenario C: burst of 3 should produce exactly 1 more send (total 2), got %d", mock.count())
	}
	t.Logf("scenario C reply: %q", mock.last().Text)

	// --- Scenario D: a rude message -> canned notice to the customer + owner ping ---
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 20, Date: int(now.Unix()), Chat: models.Chat{ID: 3003, Type: models.ChatTypePrivate},
			From: &models.User{ID: 3003}, Text: "иди нахуй урод", BusinessConnectionID: "connA",
		},
	})
	deadline = time.Now().Add(6 * time.Second)
	for mock.count() < 4 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if mock.count() < 4 {
		t.Fatalf("scenario D: expected a customer notice + an owner ping (total >=4 sends), got %d", mock.count())
	}
	sent := mock.snapshot()
	var customerNotice, ownerPing *sentMsg
	for i := range sent {
		m := &sent[i]
		if int64(m.ChatID) == 3003 {
			customerNotice = m
		}
		if int64(m.ChatID) == ownerID && strings.Contains(m.Text, "Грубость") {
			ownerPing = m
		}
	}
	if customerNotice == nil || customerNotice.Text != llmsvc.RudeNoticeText {
		t.Fatalf("scenario D: expected customer to receive the canned rude notice, got %+v", customerNotice)
	}
	if ownerPing == nil {
		t.Fatalf("scenario D: expected owner to be notified about the rude message")
	}
	t.Logf("scenario D: customer got %q, owner ping %q", customerNotice.Text, ownerPing.Text)

	// --- Scenario E: a request needing the owner's decision -> canned notice + owner ping ---
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 21, Date: int(now.Unix()), Chat: models.Chat{ID: 3004, Type: models.ChatTypePrivate},
			From: &models.User{ID: 3004}, Text: "скинь 5000 тенге до завтра, очень надо", BusinessConnectionID: "connA",
		},
	})
	deadline = time.Now().Add(6 * time.Second)
	sentBefore := mock.count()
	for mock.count() < sentBefore+2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	sent = mock.snapshot()
	var actionNotice, actionOwnerPing *sentMsg
	for i := range sent {
		m := &sent[i]
		if int64(m.ChatID) == 3004 {
			actionNotice = m
		}
		if int64(m.ChatID) == ownerID && strings.Contains(m.Text, "Нужно решение") {
			actionOwnerPing = m
		}
	}
	if actionNotice == nil || actionNotice.Text != llmsvc.ActionNoticeText {
		t.Fatalf("scenario E: expected customer to receive the canned action notice, got %+v", actionNotice)
	}
	if actionOwnerPing == nil {
		t.Fatalf("scenario E: expected owner to be notified about the pending decision")
	}
	t.Logf("scenario E: customer got %q, owner ping %q", actionNotice.Text, actionOwnerPing.Text)

	// --- Scenario F: a message left unanswered across a "restart" gets caught up ---
	const orphanChatID = int64(3005)
	if err := messagesRepo.Add(ctx, orphanChatID, false, "привет, ты тут?"); err != nil {
		t.Fatalf("scenario F: failed to seed orphan message: %v", err)
	}
	if err := chatStateRepo.TouchIncoming(ctx, orphanChatID); err != nil {
		t.Fatalf("scenario F: failed to seed chat_state: %v", err)
	}
	// No debounce.Trigger call here -- this simulates the in-memory timer
	// having died with the previous process, exactly as CatchUpPending exists for.
	sentBefore = mock.count()
	svc.CatchUpPending(ctx, b)
	deadline = time.Now().Add(6 * time.Second)
	for mock.count() < sentBefore+1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	orphanAnswered := false
	for _, m := range mock.snapshot() {
		if int64(m.ChatID) == orphanChatID {
			orphanAnswered = true
			t.Logf("scenario F: caught-up reply: %q", m.Text)
		}
	}
	if !orphanAnswered {
		t.Fatalf("scenario F: expected CatchUpPending to answer the orphaned chat_id=%d", orphanChatID)
	}

	// --- Scenario G: hitting the per-chat rate limit gets one notice, not repeated silence ---
	const limitChatID = int64(3006)
	llmCallCount := func() int {
		var n int
		_ = sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_calls WHERE chat_id = ?", limitChatID).Scan(&n)
		return n
	}
	for i, txt := range []string{"привет1", "привет2", "привет3"} {
		before := llmCallCount()
		svc.HandleUpdate(ctx, b, &models.Update{
			BusinessMessage: &models.Message{
				ID: 30 + i, Date: int(now.Unix()), Chat: models.Chat{ID: limitChatID, Type: models.ChatTypePrivate},
				From: &models.User{ID: limitChatID}, Text: txt, BusinessConnectionID: "connA",
			},
		})
		deadline = time.Now().Add(6 * time.Second)
		for llmCallCount() < before+1 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		// llm_calls is written just before the actual SendMessage call in
		// processBatch, so the reply itself may still be in flight here --
		// let it settle before the next message's Trigger captures a count.
		time.Sleep(500 * time.Millisecond)
	}
	if got := llmCallCount(); got != 3 {
		t.Fatalf("scenario G: expected 3 llm_calls before hitting CHAT_LIMIT, got %d", got)
	}

	sentBefore = mock.count()
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 40, Date: int(now.Unix()), Chat: models.Chat{ID: limitChatID, Type: models.ChatTypePrivate},
			From: &models.User{ID: limitChatID}, Text: "привет4", BusinessConnectionID: "connA",
		},
	})
	deadline = time.Now().Add(6 * time.Second)
	for mock.count() < sentBefore+1 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if got := llmCallCount(); got != 3 {
		t.Fatalf("scenario G: 4th message should be blocked before any LLM call, llm_calls=%d", got)
	}
	var noticeMsg *sentMsg
	for _, m := range mock.snapshot() {
		if int64(m.ChatID) == limitChatID {
			mCopy := m
			noticeMsg = &mCopy
		}
	}
	if noticeMsg == nil || !strings.Contains(noticeMsg.Text, "устал отвечать") {
		t.Fatalf("scenario G: expected a rate-limit notice to the customer, got %+v", noticeMsg)
	}
	t.Logf("scenario G: rate-limit notice: %q", noticeMsg.Text)

	// A 5th message in the same window must NOT get a second notice.
	sentBefore = mock.count()
	svc.HandleUpdate(ctx, b, &models.Update{
		BusinessMessage: &models.Message{
			ID: 41, Date: int(now.Unix()), Chat: models.Chat{ID: limitChatID, Type: models.ChatTypePrivate},
			From: &models.User{ID: limitChatID}, Text: "привет5", BusinessConnectionID: "connA",
		},
	})
	time.Sleep(3 * time.Second)
	if got := mock.count(); got != sentBefore {
		t.Fatalf("scenario G: expected no second rate-limit notice within the same window, got %d new send(s)", got-sentBefore)
	}
}
