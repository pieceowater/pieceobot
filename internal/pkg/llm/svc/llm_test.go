package svc

import "testing"

func TestParseModelOutput(t *testing.T) {
	stickers := map[string]string{"laugh": "FILE_ID_LAUGH", "ok": "FILE_ID_OK"}

	cases := []struct {
		name        string
		raw         string
		wantReply   string
		wantSticker string
		wantRude    bool
	}{
		{name: "plain reply", raw: "Привет! Всё хорошо", wantReply: "Привет! Всё хорошо"},
		{name: "skip", raw: "SKIP", wantReply: ""},
		{name: "skip lowercase", raw: "skip", wantReply: ""},
		{name: "skip with whitespace", raw: "  SKIP  ", wantReply: ""},
		{name: "empty", raw: "", wantReply: ""},
		{name: "rude", raw: "RUDE", wantReply: RudeNoticeText, wantRude: true},
		{name: "rude lowercase", raw: "rude", wantReply: RudeNoticeText, wantRude: true},
		{name: "known sticker tag", raw: "STICKER:laugh", wantSticker: "FILE_ID_LAUGH"},
		{name: "known sticker tag lowercase prefix", raw: "sticker:ok", wantSticker: "FILE_ID_OK"},
		{name: "unknown sticker tag falls back to skip", raw: "STICKER:nonexistent", wantReply: ""},
		{name: "text that merely mentions RUDE is a normal reply", raw: "не грубость, просто слово RUDE тут", wantReply: "не грубость, просто слово RUDE тут"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replyText, stickerFileID, isRude := parseModelOutput(tc.raw, stickers)

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

			if tc.wantSticker != "" && replyText != nil {
				t.Errorf("sticker case must not also set replyText")
			}
		})
	}
}
