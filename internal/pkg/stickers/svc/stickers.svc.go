// Package svc loads the owner's sticker-reply mapping -- a small
// "tag: file_id" text file the owner fills in by hand. Optional: a missing
// or empty file just means the bot never has a sticker to reach for, no
// error and no mention of stickers in the system prompt.
package svc

import (
	"bufio"
	"os"
	"strings"
)

// Load reads "tag: file_id" lines (blank lines and lines starting with #
// ignored). Get a file_id by sending the sticker directly to the bot in its
// own chat -- see telegram/svc's owner-sticker handler.
func Load(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tag, fileID, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		tag, fileID = strings.TrimSpace(tag), strings.TrimSpace(fileID)
		if tag != "" && fileID != "" {
			out[tag] = fileID
		}
	}
	return out, scanner.Err()
}
