package main

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/jeanpetric/assistant/internal/slack"
)

// slackClient reads the bot token from path; a missing or empty file means
// Slack is off and gives nil.
func slackClient(path string) (*slack.Client, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, nil
	}
	return &slack.Client{Token: token}, nil
}
