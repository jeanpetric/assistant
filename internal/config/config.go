// Package config loads settings from a JSON file. Command-line flags override it.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	Port       int    `json:"port"`
	DB         string `json:"db"`
	Claude     string `json:"claude"`
	ClaudeArgs string `json:"claude_args"`
	Gemini     string `json:"gemini"`
	GeminiArgs string `json:"gemini_args"`
	Workdir    string `json:"workdir"`
	// Agent runs sessions that name none: claude or gemini.
	Agent string `json:"agent"`
	Mode  string `json:"mode"`
	// Trello API key and token (https://trello.com/power-ups/admin); both
	// set turns the Trello sync on.
	TrelloKey   string `json:"trello_key"`
	TrelloToken string `json:"trello_token"`
	// SyncListen, when set (e.g. 127.0.0.1:6768), makes this node the sync
	// hub: /sync/* is served there, and only there, to signed requests.
	SyncListen string `json:"sync_listen"`
}

func Defaults(home string) Config {
	return Config{
		Port:    6767,
		DB:      filepath.Join(home, ".assistant", "assistant.db"),
		Claude:  "claude",
		Gemini:  "gemini",
		Workdir: home,
		Agent:   "claude",
		Mode:    "bypassPermissions",
	}
}

// Load reads path over the defaults. A missing file is created with the defaults
// so there is always something to edit.
func Load(path, home string) (Config, error) {
	c := Defaults(home)
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, write(path, c)
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, fmt.Errorf("%s: port %d out of range", path, c.Port)
	}
	return c, nil
}

func write(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}
