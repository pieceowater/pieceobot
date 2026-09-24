package svc

import (
	"strings"
	"testing"
)

func TestParseModelOutput(t *testing.T) {
	stickers := map[string]string{"laugh": "FILE_ID_LAUGH", "ok": "FILE_ID_OK"}

	cases := []struct {
		name        string
		raw         string
		wantReply   string
		wantSticker string
		wantRude    bool
		wantAction  bool
	}{
		{name: "plain reply", raw: "Привет! Всё хорошо", wantReply: "Привет! Всё хорошо"},
		{name: "skip", raw: "SKIP", wantReply: ""},
		{name: "skip lowercase", raw: "skip", wantReply: ""},
		{name: "skip with whitespace", raw: "  SKIP  ", wantReply: ""},
		{name: "empty", raw: "", wantReply: ""},
		{name: "rude", raw: "RUDE", wantReply: RudeNoticeText, wantRude: true},
		{name: "rude lowercase", raw: "rude", wantReply: RudeNoticeText, wantRude: true},
		{name: "rude with comeback", raw: "RUDE\nха, сам такой", wantReply: "ха, сам такой\n\n" + RudeNoticeText, wantRude: true},
		{name: "rude lowercase with comeback", raw: "rude\nну ты и наглец", wantReply: "ну ты и наглец\n\n" + RudeNoticeText, wantRude: true},
		{name: "action", raw: "ACTION", wantReply: ActionNoticeText, wantAction: true},
		{name: "action lowercase", raw: "action", wantReply: ActionNoticeText, wantAction: true},
		{name: "known sticker tag", raw: "STICKER:laugh", wantSticker: "FILE_ID_LAUGH"},
		{name: "known sticker tag lowercase prefix", raw: "sticker:ok", wantSticker: "FILE_ID_OK"},
		{name: "unknown sticker tag falls back to skip", raw: "STICKER:nonexistent", wantReply: ""},
		{name: "text that merely mentions RUDE is a normal reply", raw: "не грубость, просто слово RUDE тут", wantReply: "не грубость, просто слово RUDE тут"},
		{name: "text that merely mentions ACTION is a normal reply", raw: "не призыв, просто слово ACTION тут", wantReply: "не призыв, просто слово ACTION тут"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replyText, stickerFileID, isRude, isAction := parseModelOutput(tc.raw, stickers)

			gotReply := ""
			if replyText != nil {
				gotReply = *replyText
			}
			if gotReply != tc.wantReply {
				t.Errorf("reply = %q, want %q", gotReply, tc.wantReply)
			}

			gotSticker := ""
			if stickerFileID != nil {
				gotSticker = *stickerFileID
			}
			if gotSticker != tc.wantSticker {
				t.Errorf("sticker = %q, want %q", gotSticker, tc.wantSticker)
			}

			if isRude != tc.wantRude {
				t.Errorf("isRude = %v, want %v", isRude, tc.wantRude)
			}
			if isAction != tc.wantAction {
				t.Errorf("isAction = %v, want %v", isAction, tc.wantAction)
			}

			if tc.wantSticker != "" && replyText != nil {
				t.Errorf("sticker case must not also set replyText")
			}
		})
	}
}

func TestPrependStyleExamples(t *testing.T) {
	t.Run("no examples leaves historyText untouched", func(t *testing.T) {
		got := PrependStyleExamples("Новое сообщение — отреагируй именно на него:\nОн: привет", nil)
		want := "Новое сообщение — отреагируй именно на него:\nОн: привет"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("empty historyText stays empty regardless of examples", func(t *testing.T) {
		got := PrependStyleExamples("", []string{"го спать"})
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("examples are prefixed as Я: lines before the original text", func(t *testing.T) {
		historyText := "Новое сообщение — отреагируй именно на него:\nОн: привет"
		got := PrependStyleExamples(historyText, []string{"го спать", "го курить"})

		if !strings.HasPrefix(got, "Примеры твоих реальных сообщений") {
			t.Errorf("expected examples block header at the start, got %q", got)
		}
		if !strings.Contains(got, "Я: го спать") || !strings.Contains(got, "Я: го курить") {
			t.Errorf("expected both examples rendered as Я: lines, got %q", got)
		}
		if !strings.HasSuffix(got, historyText) {
			t.Errorf("expected the original historyText preserved verbatim at the end, got %q", got)
		}
	})
}
