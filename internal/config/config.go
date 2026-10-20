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
	Workdir    string `json:"workdir"`
	Mode       string `json:"mode"`
	Model      string `json:"model"`  // default model; empty uses the claude CLI default
	Models     string `json:"models"` // comma separated, offered in the UI
	Runner     string `json:"runner"` // tmux (interactive claude per session) | print (claude -p per turn)
	TmuxSocket string `json:"tmux_socket"`
}

func Defaults(home string) Config {
	return Config{
		Port:    6767,
		DB:      filepath.Join(home, ".assistant", "assistant.db"),
		Claude:  "claude",
		Workdir: home,
		Mode:    "bypassPermissions",
		Models:  "opus,sonnet,haiku",
		Runner:  "tmux",
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
	if c.Runner != "tmux" && c.Runner != "print" {
		return c, fmt.Errorf("%s: runner must be tmux or print, got %q", path, c.Runner)
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
