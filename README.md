# pieceobot

A Telegram Business auto-responder on Claude. Connects to your personal
account via Telegram Business → Chatbots and replies to people **as you**
in private chats: simple small talk only. Anything more complicated stays
silent — better to say nothing than to answer off-topic or out of place.

Stack: Go 1.27 + [go-telegram/bot](https://github.com/go-telegram/bot) (long
polling, no webhook/domain needed), the official Anthropic Go SDK
(`claude-sonnet-5` by default), SQLite (`modernc.org/sqlite`, pure Go, no
cgo), Docker Compose. Architecture follows `lotof.tg.capital.bot`:
`cmd/server` → `internal/app.go` (wiring) → `internal/core/cfg` →
`internal/pkg/<domain>/svc`.

The bot is hard-locked to a single Telegram account (`OWNER_USER_ID`) — if
anyone else connects it to their own Business account, the bot notices and
never does anything there (see "How it works" below).

---

## Manual setup steps

### 1. Telegram Premium

Every Telegram Business feature (including chatbots) requires an active
**Telegram Premium** subscription. Without it, the Business section in
Settings won't even appear.

### 2. @BotFather — enable Business Mode

1. Open [@BotFather](https://t.me/BotFather) → `/mybots` → pick your bot.
2. **Bot Settings** → **Business Mode** → **Turn on**.

Skip this and Telegram shows `This bot doesn't support Telegram Business
yet` when you try to add it as a chatbot.

### 3. Telegram → Settings → Telegram Business → Chatbots

1. Point it at your bot.
2. For the first test, pick "only selected contacts" rather than "all
   chats" — much easier to verify.
3. Grant the bot the **reply to messages** permission.

### 4. Anthropic API

1. Get a key at [console.anthropic.com](https://console.anthropic.com) →
   API Keys.
2. While there, set a **spend limit** (Settings → Limits) — the `.env`
   budget stops the bot itself, but the Anthropic console limit is your
   backstop if the bot ever misbehaves.

### 5. Your numeric user id

`OWNER_USER_ID` in `.env` is a numeric id, not `@username`. The easiest way
to get it is [@userinfobot](https://t.me/userinfobot) (message it `/start`).

---

## How it works

- `business_connection`, `business_message`, `edited_business_message`, and
  `deleted_business_messages` updates are explicitly listed in
  `allowed_updates`.
- The bot can't see any conversation history from before it was connected —
  it builds its own from incoming updates, stored in SQLite. That's the
  only source of context it has.
- **Owner lock.** Every `business_connection` update is checked against
  `business_connection.user.id == OWNER_USER_ID`. If someone else connects
  the bot to their own Business account, that connection is marked "not
  mine" and the bot never replies or stores history for it — no matter how
  many messages arrive. The check happens on the numeric account id, not a
  name or username.
- One LLM call per message batch — the model itself decides: reply with
  text, reply with a sticker, `SKIP` (stay completely silent), `RUDE`
  (hostile/abusive message), or `ACTION` (needs the owner's own
  decision/agreement — money, meetings, arrangements, invitations, etc.).
  The call is **streamed** (lower time-to-first-byte than a blocking
  request), though the reply is still fully assembled before it's sent —
  Telegram needs the complete text, not a live-edited message. Debounce
  waits 20s (max 60s) after the last message in a burst before calling the
  model once for the whole batch.
- The model judges tone/topic/rudeness only against the newest,
  not-yet-answered message — the rest of the history is background tone
  only, so one rude or heavy message doesn't "leak" onto unrelated ones
  that follow it.
- **Style personalization.** For a chat that already has a few of your own
  messages, the model is told to match *that specific conversation's* tone
  first, `persona.md` second. For a brand-new or barely-talked-to contact
  (fewer than 3 of your own messages in that chat), the bot instead pulls
  up to 10 of your genuinely hand-typed messages from *other* chats as
  style examples — never its own past generated replies, which are tracked
  separately precisely so they can't contaminate the sample.
- If a message comes from you (typed by hand on your phone), the bot goes
  quiet in that chat for `OWNER_ACTIVE_PAUSE_MIN` minutes. If the bot itself
  sent a message via the Business API, that does *not* count as "you're in
  the conversation" and doesn't restart the timer — otherwise the bot would
  end up silencing itself after every reply.
- **Rudeness and requests needing your decision aren't just dropped.** For
  a rude message, the model replies "Ваше сообщение передано владельцу
  этого автоответчика." ("Your message has been passed to the owner of
  this auto-responder"); for something needing your call, "Я
  автоответчик — спрошу у владельца, передал инфу." ("I'm a bot, I'll ask
  the owner, passed it along"). Either way you always get notified (with
  the full message text and the contact's name), regardless of
  `NOTIFY_ON_SKIP`.
- **Chat rate limit notice.** If a single chat hits its own reply limit
  (`CHAT_LIMIT` per `CHAT_WINDOW_SEC`, default 3 per 5 minutes), the
  customer gets one static, non-LLM-generated notice ("Я бот, устал
  отвечать тебе — отвечу, когда лимит освободится.") instead of pure
  silence on every message while limited — sent at most once per window, so
  a chatty burst doesn't get spammed with the notice too.
- **Restart resilience.** The debounce timer lives only in the process's
  memory, so a restart landing inside a customer's open debounce window
  would normally drop that reply silently. On startup, `CatchUpPending`
  finds every chat whose last message is still unanswered
  (`chat_state.last_message_from_me = 0`) and reprocesses it through the
  normal pipeline — same rate limits, same mute/owner-active guards.

---

## Deploy

```bash
git clone <repo> pieceobot && cd pieceobot
cp .env.example .env
# fill in BOT_TOKEN, ANTHROPIC_API_KEY, OWNER_USER_ID
nano persona.md   # describe your own texting style, 5-10 bullet points
docker compose up -d --build
docker compose logs -f bot   # confirm it started and is receiving updates
```

Without Docker (local run): `go build ./cmd/server && ./server` (or `make run`).

### Updating

```bash
git pull
docker compose up -d --build
```

The SQLite file lives at `./data/bot.db` on the host (a volume) — it
survives rebuilds and container restarts.

---

## Configuration (`.env`)

Full list with comments in `.env.example`. The highlights:

| Variable | What it controls |
|---|---|
| `DEBOUNCE_SEC` / `DEBOUNCE_MAX_SEC` | How long to wait for a message burst to settle before replying |
| `GLOBAL_LIMIT` / `CHAT_LIMIT` / `CHAT_DAILY_LIMIT` | LLM-call ceilings (sliding windows) |
| `DAILY_BUDGET_USD` / `MONTHLY_BUDGET_USD` | Daily/monthly spend cap — the bot auto-pauses once hit |
| `OWNER_ACTIVE_PAUSE_MIN` | How many minutes the bot stays quiet after your own manual message in a chat |
| `HISTORY_TTL_DAYS` | How many days other people's messages are kept before deletion |
| `BLACKLIST` / `WHITELIST` | Comma-separated user ids |

To change a value: edit `.env` → `docker compose up -d` (recreates the
container with the new environment, no rebuild needed).

### Persona (`persona.md`)

5-10 lines describing how you actually text people — mixed into the system
prompt, and the model is explicitly told to hold to that style strictly
(sentence length, punctuation, forms of address, emoji) rather than
drifting into a neutral, generic-assistant tone. Real conversation history
in a given chat still wins when there's enough of it — persona.md is the
baseline for chats that don't have much history yet (see "Style
personalization" above).

**Until `persona.md` is filled in**, only the base rules apply, with no
personal tuning. Fill it in for replies that actually sound like you.

Changes require a restart: `docker compose restart bot`.

### Stickers (`stickers.md`)

The bot can reply with a sticker instead of text when that fits the
conversation. The list is `tag: file_id`, one per line (`#` starts a
comment):

```
laugh: CAACAgIAAxkBAAI...
ok: CAACAgIAAxkBAAI...
```

To find a sticker's `file_id`, send it **directly to the bot** (the same
private chat you use for commands) from the `OWNER_USER_ID` account — the
bot replies with its `file_id`. Without `stickers.md` (or with an empty
one), the bot doesn't even know stickers are an option — nothing about them
is added to the prompt. Changes require a restart.

---

## Control (commands sent directly to the bot, not via Business)

Message the bot in a regular private chat (not the Business chat with a
customer), from the `OWNER_USER_ID` account. Anyone else's commands are
silently ignored.

- `/pause`, `/resume` — global on/off.
- `/mute <user_id>` (or by replying to a notification shaped like
  `(id 123)`), `/unmute <user_id>` — per chat.
- `/stats` — replies today / calls in the last 2 min, tokens in/out, spend
  today and this month, remaining daily budget.
- `/budget <usd>` — change the daily limit on the fly, no `.env` edit
  needed.

Skip/rude/action-needed notifications show the customer's captured display
name (falling back to `id N` if it hasn't been captured yet) instead of a
bare id or a `tg://` link — neither a plain-text `tg://` link nor an inline
"open chat" button render reliably across Telegram clients (the button
could even fail the whole notification outright on some accounts' privacy
settings). The numeric id stays in parentheses purely so `/mute`/`/unmute`
can still parse a target out of a replied-to notification.

---

## Logs

```bash
docker compose logs -f bot
```

Only metadata is logged (chat_id, decision, token counts) — **message text
is never written to logs**, only to SQLite.

---

## Code layout

```
cmd/server/main.go                        # entrypoint, graceful shutdown (SIGINT/SIGTERM)
internal/app.go                           # wiring: every service, the single default handler
internal/core/cfg/cfg.go                  # Config from .env, fail-fast on required fields
internal/pkg/storage/repo/                # sqlite schema + repositories (messages, llm_calls, chat_state, ...)
internal/pkg/limiter/svc/                 # sliding-window rate limits + budget
internal/pkg/debounce/svc/                # message-burst debounce (time.AfterFunc)
internal/pkg/persona/svc/                 # system prompt + persona.md
internal/pkg/stickers/svc/                # stickers.md (tag -> file_id)
internal/pkg/llm/svc/                     # the one streamed Anthropic call, SKIP/RUDE/ACTION/STICKER parsing, cost calc, style examples
internal/pkg/telegram/svc/telegram.svc.go # business_connection/business_message pipeline
internal/pkg/telegram/svc/commands.go     # /pause /resume /mute /unmute /stats /budget
```

The same `cmd/server` + `internal/core` + `internal/pkg/<domain>/svc`
layering as `lotof.tg.capital.bot`: `cfg.Inst()` is the config singleton,
`app.go` is the one place that wires every service together, each domain
gets its own `svc` package. Unlike the reference bot (which talks to
Telegram with a bare HTTP client — all it ever needs is one `sendMessage`
call), this bot needs full long polling with
`business_connection`/`business_message` handling, so it uses
`go-telegram/bot` instead of a hand-rolled client — but the update
dispatcher (`Service.HandleUpdate` in telegram.svc.go) stays the single
entry point, playing the same role the reference bot's `Job`/`Scheduler`
does.

`internal/smoke_test.go` is an end-to-end pipeline test (owner lock,
debounce, rude/action canned replies + owner pings, sticker replies,
rate-limit notice, restart catch-up, manual-vs-bot-generated message
isolation) against the real Anthropic API with a mock Telegram server;
it's silently skipped (`t.Skip`) if `ANTHROPIC_API_KEY` isn't set.
`internal/pkg/llm/svc/llm_test.go` is a pure, no-network unit test of the
SKIP/RUDE/ACTION/`STICKER:<tag>` parsing and the style-examples prompt
assembly. `go test ./...` — without a key the smoke test skips, everything
else runs as usual.

---

## Acceptance testing

Verified automatically (real Claude calls through the full pipeline via
`internal/smoke_test.go`, plus manual testing during development):

1. ✅ "Привет, как дела?" ("Hi, how's it going?") → a short, on-topic
   reply. Confirmed live on a real Business connection too.
2. ✅ "Скинь 5000 тенге до завтра" ("Send me 5000 tenge by tomorrow") →
   `ACTION`, the customer gets the "I'll ask the owner" notice, nothing is
   decided on the owner's behalf.
3. ✅ "Ты бот?" ("Are you a bot?") → `SKIP`, full silence.
4. ✅ A burst of several messages in a row → exactly one model call
   (debounce); in production it settled in ~20s as configured.
5. ✅ Owner lock: a `business_connection` from someone else's account →
   zero replies sent, however many messages arrive there.
6. ✅ Rude/hostile message → the customer gets the canned notice, the owner
   gets a ping with the full text and the contact's name — confirmed live
   and in the smoke test.
7. ✅ Rudeness in history doesn't "leak" onto the next, unrelated message
   from the same contact (found and fixed live: "го курить" right after
   "пошел нахуй" was incorrectly flagged RUDE by association, until the
   decision was scoped to only the newest message).
8. ✅ Rate limit/budget: sliding-window counters and budget auto-pause —
   code review + manual verification, plus a dedicated smoke-test scenario
   for the per-chat notice and its once-per-window dedup.
9. ✅ Restart: SQLite on a volume, `PRAGMA journal_mode=WAL`, limits and
   history survive a container restart. A restart landing inside an open
   debounce window (e.g. a redeploy mid-conversation) used to drop that
   reply silently — `CatchUpPending` now finds and reprocesses any such
   chat on startup, confirmed live after two real redeploy collisions
   during testing.
10. ✅ A bare incoming sticker never gets silently dropped — it gets either
    a matching sticker from `stickers.md` or, failing that, a fallback
    emoji drawn from persona.md's own whitelist (never a generic smiley) —
    confirmed live in production.

Requires a real Business connection (verify by hand after deploying):

11. You message the chat yourself → the bot stays quiet for 30 minutes.
12. Incoming voice message → stored as `[голосовое]` in history, no reply
    attempted on its own (only stickers get the guaranteed-reply treatment).
13. A burst of spam across different chats → no more than `GLOBAL_LIMIT`
    calls within `GLOBAL_WINDOW_SEC`.
14. `DAILY_BUDGET_USD=0.001` → the bot pauses itself and sends a
    notification.
15. `/stats` — cross-check the numbers against the Anthropic console.

---

## Troubleshooting

**"This bot doesn't support Telegram Business yet"** when adding it as a
chatbot → Business Mode isn't enabled in BotFather (see step 2 above).

**The bot doesn't respond to anything at all** → check: Premium
subscription is active, the "reply to messages" permission is granted in
Business settings, `OWNER_USER_ID` in `.env` matches your real numeric id
(not a username), the container isn't paused (`/stats` will show it).

**`` `temperature` is deprecated for this model ``** → current-generation
models (Sonnet 5, Opus 5+) reject the `temperature` parameter outright;
older ones like Haiku 4.5 merely accept it. The code no longer sends it at
all, which works on every model — if you see this error, you're running an
older build.
