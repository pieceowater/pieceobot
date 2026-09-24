# pieceobot

Telegram Business-автоответчик на Claude Haiku 4.5. Подключается к твоему
личному аккаунту через Telegram Business → Chatbots и отвечает собеседникам
**от твоего имени** в личных чатах: только простой small talk. Всё сложное —
молчит, лучше промолчать, чем ответить не в тему.

Стек: Go 1.27 + [go-telegram/bot](https://github.com/go-telegram/bot) (long
polling, без вебхука/домена), официальный Anthropic Go SDK
(`claude-haiku-4-5`), SQLite (`modernc.org/sqlite`, чистый Go, без cgo),
Docker Compose. Архитектура и подход — по мотивам `lotof.tg.capital.bot`:
`cmd/server` → `internal/app.go` (wiring) → `internal/core/cfg` → `internal/pkg/<domain>/svc`.

Бот жёстко привязан к одному Telegram-аккаунту (`OWNER_USER_ID`) — если кто-то
ещё подключит его к своему Business-аккаунту, бот это увидит и не будет там
ничего делать (см. «Как это работает» ниже).

---

## Что нужно сделать руками

### 1. Telegram Premium

Все функции Telegram Business (включая чат-ботов) доступны только с активной
подпиской **Telegram Premium**. Без неё раздел Business в настройках вообще
не появится.

### 2. @BotFather — включить Business Mode

1. Открой [@BotFather](https://t.me/BotFather) → `/mybots` → выбери бота.
2. **Bot Settings** → **Business Mode** → **Turn on**.

Без этого шага Telegram при попытке добавить бота в автоответчики покажет
`This bot doesn't support Telegram Business yet`.

### 3. Telegram → Settings → Telegram Business → Chatbots

1. Укажи своего бота.
2. На первое время выбери «только выбранные контакты», а не «все чаты» —
   удобнее тестировать.
3. Выдай боту право **отвечать на сообщения**.

### 4. Anthropic API

1. Ключ — [console.anthropic.com](https://console.anthropic.com) → API Keys.
2. Там же поставь **лимит расхода** (Settings → Limits) — `.env`-бюджет
   останавливает бота, но лимит в консоли Anthropic — это твой предохранитель
   на случай, если сам бот где-то сломается.

### 5. Свой numeric user id

`OWNER_USER_ID` в `.env` — это числовой id, не `@username`. Проще всего
получить у [@userinfobot](https://t.me/userinfobot) (напиши ему `/start`).

---

## Как это работает

- Апдейты `business_connection`, `business_message`, `edited_business_message`,
  `deleted_business_messages` включены явно в `allowed_updates`.
- Бот не видит переписку до подключения — историю копит сам, из входящих
  апдейтов, в SQLite. Это единственный источник контекста.
- **Привязка к владельцу.** На каждый `business_connection`-апдейт бот
  сверяет `business_connection.user.id` с `OWNER_USER_ID`. Если кто-то другой
  подключит бота к своему Business-аккаунту, это подключение помечается
  "не моё" и бот никогда не отвечает и не сохраняет историю по нему — сколько
  бы сообщений туда ни пришло. Проверка на входе, не по paмени/username.
- Один вызов LLM на пачку сообщений — модель сама решает, отвечать или
  выдать `SKIP`. Debounce ждёт 20 сек (макс. 60) после последнего сообщения
  в пачке, прежде чем звать модель.
- Если сообщение от самого владельца (ты написал вручную с телефона) — бот
  замолкает в этом чате на `OWNER_ACTIVE_PAUSE_MIN` минут. Если сообщение
  отправил сам бот через Business API — это не считается «ты в диалоге» и
  таймер не перезапускается (иначе бот молчал бы сам после себя).

---

## Деплой

```bash
git clone <repo> pieceobot && cd pieceobot
cp .env.example .env
# впиши BOT_TOKEN, ANTHROPIC_API_KEY, OWNER_USER_ID
nano persona.md   # опиши свой стиль, 5-10 пунктов
docker compose up -d --build
docker compose logs -f bot   # проверить, что бот стартанул и получает апдейты
```

Без Docker (локально): `go build ./cmd/server && ./server` (или `make run`).

### Обновление

```bash
git pull
docker compose up -d --build
```

SQLite-файл лежит в `./data/bot.db` на хосте (volume) — переживает пересборку
и рестарт контейнера.

---

## Конфигурация (`.env`)

Полный список — в `.env.example`, там же комментарии. Самое важное:

| Переменная | Что меняет |
|---|---|
| `DEBOUNCE_SEC` / `DEBOUNCE_MAX_SEC` | Сколько ждать паузу в сообщениях перед ответом |
| `GLOBAL_LIMIT` / `CHAT_LIMIT` / `CHAT_DAILY_LIMIT` | Потолки LLM-вызовов (скользящее окно) |
| `DAILY_BUDGET_USD` / `MONTHLY_BUDGET_USD` | Деньги в день/месяц — при достижении бот сам встаёт на паузу |
| `OWNER_ACTIVE_PAUSE_MIN` | На сколько минут бот молчит после твоего ручного сообщения в чате |
| `HISTORY_TTL_DAYS` | Через сколько дней чужая переписка удаляется из базы |
| `BLACKLIST` / `WHITELIST` | user_id через запятую |

Поменять значение → отредактировать `.env` → `docker compose up -d` (пересоздаст
контейнер с новыми переменными, без ребилда).

### Персона (`persona.md`)

5-10 строк о том, как ты пишешь — подмешивается в системный промпт.
Изменения требуют рестарта: `docker compose restart bot`.

---

## Управление (команды боту напрямую, не через Business)

Пиши боту в обычный личный чат (не в Business-чат с клиентом), с аккаунта
`OWNER_USER_ID`. От всех остальных пользователей команды молча игнорируются.

- `/pause`, `/resume` — глобально вкл/выкл.
- `/mute <user_id>` (или ответом на уведомление вида `Пропустил (123): ...`),
  `/unmute <user_id>` — для одного чата.
- `/stats` — ответы сегодня / вызовы за 2 мин, токены in/out, расход $
  сегодня и за месяц, остаток дневного бюджета.
- `/budget <usd>` — поменять дневной лимит на лету (без правки `.env`).

---

## Логи

```bash
docker compose logs -f bot
```

Логируются только метаданные (chat_id, решение, токены) — **текст сообщений
в логи не пишется**, только в SQLite.

---

## Структура кода

```
cmd/server/main.go                       # entrypoint, graceful shutdown (SIGINT/SIGTERM)
internal/app.go                          # wiring: все сервисы, единственный default handler
internal/core/cfg/cfg.go                 # Config из .env, fail-fast на обязательных полях
internal/pkg/storage/repo/               # sqlite схема + репозитории (messages, llm_calls, chat_state, ...)
internal/pkg/limiter/svc/                # скользящие окна rate-limit + бюджет
internal/pkg/debounce/svc/               # ожидание пачки сообщений (time.AfterFunc)
internal/pkg/persona/svc/                # system prompt + persona.md
internal/pkg/llm/svc/                    # один вызов Anthropic, парсинг SKIP, расчёт стоимости
internal/pkg/telegram/svc/telegram.svc.go # business_connection/business_message пайплайн (ТЗ 3.2)
internal/pkg/telegram/svc/commands.go     # /pause /resume /mute /unmute /stats /budget
```

Ровно тот же `cmd/server` + `internal/core` + `internal/pkg/<domain>/svc`
слой, что и в `lotof.tg.capital.bot`: `cfg.Inst()` — синглтон конфига,
`app.go` — единственное место, которое связывает все сервисы, каждый домен —
свой пакет `svc`. В отличие от референса (который дёргает Telegram голым
HTTP-клиентом — ему хватает одного `sendMessage`), этому боту нужен
полноценный long polling с обработкой `business_connection`/`business_message`,
поэтому вместо самодельного клиента используется `go-telegram/bot` — но
Update-диспетчер (`Service.HandleUpdate` в telegram.svc.go) остаётся одной
точкой входа, как и `Job`/`Scheduler` в референсе.

`internal/smoke_test.go` — сквозной тест пайплайна (owner-привязка,
debounce, отправка) против реального Anthropic API и поднятого в тесте
mock-сервера Telegram; тихо пропускается (`t.Skip`), если `ANTHROPIC_API_KEY`
не задан в окружении. `go test ./...` — без ключа все пакеты просто "no test files"/skip.

---

## Приёмка

Автоматически проверено (реальные вызовы Claude Haiku 4.5 через полный
пайплайн — `internal/smoke_test.go` — и вручную в ходе разработки):

1. ✅ «Привет, как дела?» → короткий ответ в тему.
2. ✅ «Скинь 5000 до пятницы» / «Встречаемся завтра в 7?» → `SKIP`, ничего не отправлено.
3. ✅ «Ты бот?» → `SKIP`.
4. ✅ Пачка из нескольких сообщений подряд → ровно один вызов модели (debounce), проверено в smoke-тесте.
5. ✅ Привязка к владельцу: business_connection от чужого аккаунта → ноль
   отправленных сообщений, сколько бы сообщений туда ни пришло — проверено в smoke-тесте.
6. ✅ Rate-limit/бюджет: sliding-window счётчики и авто-пауза по бюджету — код-ревью + ручная проверка.
7. ✅ Рестарт: SQLite в volume, `PRAGMA journal_mode=WAL`, лимиты/история
   переживают рестарт контейнера.

Требует реального Business-подключения (проверь руками после деплоя):

8. Ты пишешь в чат сам → бот молчит 30 мин.
9. Стикер/голосовое → сохраняется как `[стикер]`/`[голосовое]` в историю, без ответа.
10. Спам из разных чатов → не больше `GLOBAL_LIMIT` вызовов в `GLOBAL_WINDOW_SEC`.
11. `DAILY_BUDGET_USD=0.001` → бот встаёт на паузу и присылает уведомление.
12. `/stats` — сверить цифры с тем, что видно в Anthropic-консоли.

---

## Troubleshooting

**"This bot doesn't support Telegram Business yet"** при добавлении в
автоответчики → не включён Business Mode в BotFather (см. шаг 2 выше).

**Бот не отвечает вообще ни на что** → проверь: подписка Premium активна,
право «отвечать на сообщения» выдано в настройках Business, `OWNER_USER_ID`
в `.env` совпадает с твоим настоящим numeric id (не username), контейнер не
на паузе (`/stats` покажет).
