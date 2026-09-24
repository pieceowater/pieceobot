// Package cfg holds the bot's environment configuration -- one Config built
// once from the environment, required keys fail startup instead of silently
// defaulting. Mirrors lotof.tg.capital.bot's internal/core/cfg.
package cfg

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken        string
	AnthropicAPIKey string
	OwnerUserID     int64

	LLMModel        string
	MaxOutputTokens int64

	HistoryMessages  int
	HistoryMaxTokens int

	DebounceSec    int
	DebounceMaxSec int

	GlobalLimit     int
	GlobalWindowSec int
	ChatLimit       int
	ChatWindowSec   int
	ChatDailyLimit  int

	OwnerActivePauseMin int

	DailyBudgetUSD     float64
	MonthlyBudgetUSD   float64
	PriceInputPerMTok  float64
	PriceOutputPerMTok float64

	NotifyOnSkip   bool
	HistoryTTLDays int

	Blacklist map[int64]struct{}
	Whitelist map[int64]struct{}

	DBPath       string
	PersonaPath  string
	StickersPath string
}

var (
	instance *Config
	once     sync.Once
)

func Inst() *Config {
	once.Do(func() {
		if err := godotenv.Load(); err != nil {
			fmt.Println("No .env file found, loading from OS environment variables.")
		}

		instance = &Config{
			BotToken:        getRequiredEnv("BOT_TOKEN"),
			AnthropicAPIKey: getRequiredEnv("ANTHROPIC_API_KEY"),
			OwnerUserID:     getRequiredInt64("OWNER_USER_ID"),

			LLMModel:        getEnv("LLM_MODEL", "claude-haiku-4-5"),
			MaxOutputTokens: getEnvInt64("MAX_OUTPUT_TOKENS", 150),

			HistoryMessages:  getEnvInt("HISTORY_MESSAGES", 12),
			HistoryMaxTokens: getEnvInt("HISTORY_MAX_TOKENS", 1500),

			DebounceSec:    getEnvInt("DEBOUNCE_SEC", 20),
			DebounceMaxSec: getEnvInt("DEBOUNCE_MAX_SEC", 60),

			GlobalLimit:     getEnvInt("GLOBAL_LIMIT", 10),
			GlobalWindowSec: getEnvInt("GLOBAL_WINDOW_SEC", 120),
			ChatLimit:       getEnvInt("CHAT_LIMIT", 3),
			ChatWindowSec:   getEnvInt("CHAT_WINDOW_SEC", 300),
			ChatDailyLimit:  getEnvInt("CHAT_DAILY_LIMIT", 200),

			OwnerActivePauseMin: getEnvInt("OWNER_ACTIVE_PAUSE_MIN", 30),

			DailyBudgetUSD:     getEnvFloat("DAILY_BUDGET_USD", 1.5),
			MonthlyBudgetUSD:   getEnvFloat("MONTHLY_BUDGET_USD", 15.0),
			PriceInputPerMTok:  getEnvFloat("PRICE_INPUT_PER_MTOK", 1.00),
			PriceOutputPerMTok: getEnvFloat("PRICE_OUTPUT_PER_MTOK", 5.00),

			NotifyOnSkip:   getEnvBool("NOTIFY_ON_SKIP", true),
			HistoryTTLDays: getEnvInt("HISTORY_TTL_DAYS", 7),

			Blacklist: getIDSet("BLACKLIST"),
			Whitelist: getIDSet("WHITELIST"),

			DBPath:       getEnv("DB_PATH", "data/bot.db"),
			PersonaPath:  getEnv("PERSONA_PATH", "persona.md"),
			StickersPath: getEnv("STICKERS_PATH", "stickers.md"),
		}
	})
	return instance
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// getRequiredEnv fails startup instead of silently falling back to a
// hardcoded default for a value with no safe default.
func getRequiredEnv(key string) string {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return value
}

func getRequiredInt64(key string) int64 {
	raw := getRequiredEnv(key)
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		log.Fatalf("environment variable %s must be an integer, got %q", key, raw)
	}
	return v
}

func getEnvInt(key string, defaultValue int) int {
	raw, exists := os.LookupEnv(key)
	if !exists || raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		log.Fatalf("environment variable %s must be an integer, got %q", key, raw)
	}
	return v
}

func getEnvInt64(key string, defaultValue int64) int64 {
	raw, exists := os.LookupEnv(key)
	if !exists || raw == "" {
		return defaultValue
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		log.Fatalf("environment variable %s must be an integer, got %q", key, raw)
	}
	return v
}

func getEnvFloat(key string, defaultValue float64) float64 {
	raw, exists := os.LookupEnv(key)
	if !exists || raw == "" {
		return defaultValue
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		log.Fatalf("environment variable %s must be a number, got %q", key, raw)
	}
	return v
}

func getEnvBool(key string, defaultValue bool) bool {
	raw, exists := os.LookupEnv(key)
	if !exists || raw == "" {
		return defaultValue
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Fatalf("environment variable %s must be a boolean, got %q", key, raw)
		return false
	}
}

func getIDSet(key string) map[int64]struct{} {
	ids := make(map[int64]struct{})
	raw := os.Getenv(key)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			log.Fatalf("environment variable %s: %q is not a valid id", key, part)
		}
		ids[v] = struct{}{}
	}
	return ids
}
