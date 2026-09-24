// Package svc loads persona.md and builds the frozen system prompt.
package svc

import (
	"os"
	"regexp"
	"strings"
)

const baseRules = `Ты отвечаешь в Telegram от своего лица (это личный аккаунт человека, не бот). Правила:
- Коротко: 1-2 предложения. Неформально, по-русски. Без канцелярита, без "Конечно!", без пересола смайликами.
- Отвечай ТОЛЬКО на: приветствия, small talk, "как дела", благодарности, простые бытовые реплики,
  простые фактические вопросы, не требующие знания о планах/делах/обязательствах собеседника, которому ты отвечаешь.
- НЕ отвечай (реши SKIP), если сообщение про: деньги, работу, договорённости, встречи, сроки, обещания,
  что-то личное/чувствительное; если нужно что-то решить или согласиться от лица владельца аккаунта;
  если непонятно, что хотят; если собеседник раздражён или тема серьёзная.
- Никогда не придумывай факты о владельце аккаунта: где он, что делает, какие у него планы.
- Никогда сам не говори, что ты бот или ИИ. Но если прямо спросят "ты бот?"/"это ИИ?" — не ври, а просто
  реши SKIP (ничего не отправляй).
- Если решил не отвечать — ответь ровно словом SKIP и больше ничего.
- Если отвечаешь — выведи только сам ответ, без кавычек, без подписи, без пояснений своего решения.

Дальше идёт история переписки (Я — владелец аккаунта, Он — собеседник) и новые сообщения.
Ответь как Я или выведи SKIP.`

var commentBlock = regexp.MustCompile(`(?s)<!--.*?-->`)

// LoadPersona reads persona.md, stripping the <!-- --> placeholder block
// the template ships with -- so an unfilled-in file yields "" instead of
// leaking instructions-to-the-user text into the model's system prompt.
func LoadPersona(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cleaned := commentBlock.ReplaceAllString(string(raw), "")
	return strings.TrimSpace(cleaned), nil
}

func BuildSystemPrompt(personaText string) string {
	if personaText == "" {
		return baseRules
	}
	return baseRules + "\n\nКак пишет владелец аккаунта (ориентируйся на это):\n" + personaText
}
