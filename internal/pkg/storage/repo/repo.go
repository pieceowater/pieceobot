package repo

import (
	"context"
	"database/sql"
	"time"
)

type Message struct {
	FromMe bool
	Text   string
	TS     int64
}

type MessagesRepo struct{ db *sql.DB }

func NewMessagesRepo(db *sql.DB) *MessagesRepo { return &MessagesRepo{db: db} }

func (r *MessagesRepo) Add(ctx context.Context, chatID int64, fromMe bool, text string) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO messages (chat_id, from_me, text, ts) VALUES (?, ?, ?, ?)",
		chatID, boolToInt(fromMe), text, time.Now().Unix(),
	)
	return err
}

// Recent returns the last `limit` messages for chatID, oldest first.
func (r *MessagesRepo) Recent(ctx context.Context, chatID int64, limit int) ([]Message, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT from_me, text, ts FROM messages WHERE chat_id = ? ORDER BY ts DESC, id DESC LIMIT ?",
		chatID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var fromMe int
		var m Message
		if err := rows.Scan(&fromMe, &m.Text, &m.TS); err != nil {
			return nil, err
		}
		m.FromMe = fromMe != 0
		out = append(out, m)
	}
	// reverse to oldest-first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

func (r *MessagesRepo) DeleteOlderThan(ctx context.Context, cutoffTS int64) (int64, error) {
	res, err := r.db.ExecContext(ctx, "DELETE FROM messages WHERE ts < ?", cutoffTS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

type ChatState struct {
	Muted             bool
	LastOwnerMsgTS    *int64
	LastMessageFromMe bool
}

type ChatStateRepo struct{ db *sql.DB }

func NewChatStateRepo(db *sql.DB) *ChatStateRepo { return &ChatStateRepo{db: db} }

func (r *ChatStateRepo) Get(ctx context.Context, chatID int64) (ChatState, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT muted, last_owner_msg_ts, last_message_from_me FROM chat_state WHERE chat_id = ?", chatID)
	var muted, fromMe int
	var lastOwnerTS sql.NullInt64
	err := row.Scan(&muted, &lastOwnerTS, &fromMe)
	if err == sql.ErrNoRows {
		return ChatState{}, nil
	}
	if err != nil {
		return ChatState{}, err
	}
	state := ChatState{Muted: muted != 0, LastMessageFromMe: fromMe != 0}
	if lastOwnerTS.Valid {
		state.LastOwnerMsgTS = &lastOwnerTS.Int64
	}
	return state, nil
}

func (r *ChatStateRepo) upsert(ctx context.Context, chatID int64, apply func(*ChatState)) error {
	state, err := r.Get(ctx, chatID)
	if err != nil {
		return err
	}
	apply(&state)
	var lastOwnerTS any
	if state.LastOwnerMsgTS != nil {
		lastOwnerTS = *state.LastOwnerMsgTS
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO chat_state (chat_id, muted, last_owner_msg_ts, last_message_from_me)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET muted=excluded.muted,
		   last_owner_msg_ts=excluded.last_owner_msg_ts,
		   last_message_from_me=excluded.last_message_from_me`,
		chatID, boolToInt(state.Muted), lastOwnerTS, boolToInt(state.LastMessageFromMe),
	)
	return err
}

func (r *ChatStateRepo) SetMuted(ctx context.Context, chatID int64, muted bool) error {
	return r.upsert(ctx, chatID, func(s *ChatState) { s.Muted = muted })
}

// TouchOwnerManualMessage: the real owner typed this themselves (not the
// bot) -- see telegram/svc's business-message handler for why this differs
// from TouchOutgoing.
func (r *ChatStateRepo) TouchOwnerManualMessage(ctx context.Context, chatID int64) error {
	now := time.Now().Unix()
	return r.upsert(ctx, chatID, func(s *ChatState) {
		s.LastOwnerMsgTS = &now
		s.LastMessageFromMe = true
	})
}

// TouchOutgoing: any outgoing message (owner manual or this bot) -- for the
// "last message in chat is mine" rule.
func (r *ChatStateRepo) TouchOutgoing(ctx context.Context, chatID int64) error {
	return r.upsert(ctx, chatID, func(s *ChatState) { s.LastMessageFromMe = true })
}

func (r *ChatStateRepo) TouchIncoming(ctx context.Context, chatID int64) error {
	return r.upsert(ctx, chatID, func(s *ChatState) { s.LastMessageFromMe = false })
}

type SettingsRepo struct{ db *sql.DB }

func NewSettingsRepo(db *sql.DB) *SettingsRepo { return &SettingsRepo{db: db} }

func (r *SettingsRepo) Get(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := r.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (r *SettingsRepo) Set(ctx context.Context, key, value string) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value)
	return err
}

func (r *SettingsRepo) IsPaused(ctx context.Context) (bool, error) {
	value, ok, err := r.Get(ctx, "paused")
	if err != nil || !ok {
		return false, err
	}
	return value == "1", nil
}

func (r *SettingsRepo) SetPaused(ctx context.Context, paused bool) error {
	value := "0"
	if paused {
		value = "1"
	}
	return r.Set(ctx, "paused", value)
}

type BusinessConnRow struct {
	OwnerUserID int64
	IsOwner     bool
	IsEnabled   bool
	CanReply    bool
}

type BusinessConnectionsRepo struct{ db *sql.DB }

func NewBusinessConnectionsRepo(db *sql.DB) *BusinessConnectionsRepo {
	return &BusinessConnectionsRepo{db: db}
}

func (r *BusinessConnectionsRepo) Upsert(ctx context.Context, connID string, ownerUserID int64, isOwner, isEnabled, canReply bool) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO business_connections (id, owner_user_id, is_owner, is_enabled, can_reply, updated_ts)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET owner_user_id=excluded.owner_user_id,
		   is_owner=excluded.is_owner, is_enabled=excluded.is_enabled,
		   can_reply=excluded.can_reply, updated_ts=excluded.updated_ts`,
		connID, ownerUserID, boolToInt(isOwner), boolToInt(isEnabled), boolToInt(canReply), time.Now().Unix(),
	)
	return err
}

func (r *BusinessConnectionsRepo) Get(ctx context.Context, connID string) (*BusinessConnRow, error) {
	row := r.db.QueryRowContext(ctx,
		"SELECT owner_user_id, is_owner, is_enabled, can_reply FROM business_connections WHERE id = ?", connID)
	var out BusinessConnRow
	var isOwner, isEnabled, canReply int
	err := row.Scan(&out.OwnerUserID, &isOwner, &isEnabled, &canReply)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.IsOwner, out.IsEnabled, out.CanReply = isOwner != 0, isEnabled != 0, canReply != 0
	return &out, nil
}

type LlmCallsRepo struct{ db *sql.DB }

func NewLlmCallsRepo(db *sql.DB) *LlmCallsRepo { return &LlmCallsRepo{db: db} }

func (r *LlmCallsRepo) Record(ctx context.Context, chatID, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int64, costUSD float64, result string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO llm_calls (ts, chat_id, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, cost_usd, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), chatID, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens, costUSD, result,
	)
	return err
}

func (r *LlmCallsRepo) CountSince(ctx context.Context, cutoffTS int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_calls WHERE ts >= ?", cutoffTS).Scan(&n)
	return n, err
}

func (r *LlmCallsRepo) CountSinceForChat(ctx context.Context, cutoffTS, chatID int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_calls WHERE ts >= ? AND chat_id = ?", cutoffTS, chatID).Scan(&n)
	return n, err
}

func (r *LlmCallsRepo) CostSince(ctx context.Context, cutoffTS int64) (float64, error) {
	var cost float64
	err := r.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(cost_usd), 0) FROM llm_calls WHERE ts >= ?", cutoffTS).Scan(&cost)
	return cost, err
}

func (r *LlmCallsRepo) TokensSince(ctx context.Context, cutoffTS int64) (inTok, outTok int64, err error) {
	err = r.db.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0) FROM llm_calls WHERE ts >= ?",
		cutoffTS).Scan(&inTok, &outTok)
	return inTok, outTok, err
}

// SentCountSince counts calls that actually produced an outbound message --
// a real reply, a rude-notice reply, or a sticker, but not a silent skip/error.
func (r *LlmCallsRepo) SentCountSince(ctx context.Context, cutoffTS int64) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM llm_calls WHERE ts >= ? AND result IN ('sent', 'rude', 'sticker')", cutoffTS).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
