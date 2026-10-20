// Package config loads settings from a JSON file. Command-line flags override it.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Config struct {
	Port       int    `json:"port"`
	DB         string `json:"db"`
	Claude     string `json:"claude"`
	ClaudeArgs string `json:"claude_args"`
	Workdir    string `json:"workdir"`
	Mode       string `json:"mode"`
	// Workspace is scanned for git repositories when Workdirs is unset.
	Workspace string `json:"workspace"`
	// Workdirs are offered when picking a working directory; nil means
	// "every repository in Workspace".
	Workdirs []string `json:"workdirs,omitempty"`
}

func Defaults(home string) Config {
	return Config{
		Port:      6767,
		DB:        filepath.Join(home, ".assistant", "assistant.db"),
		Claude:    "claude",
		Workdir:   home,
		Mode:      "bypassPermissions",
		Workspace: filepath.Join(home, "workspace"),
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

func write(path string, c any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// Repos lists the git repositories directly inside root, sorted. Linked
// worktrees (a .git file rather than a directory) are skipped.
func Repos(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// saveWorkdirs sets workdirs in the file at path, leaving every other key as written.
func saveWorkdirs(path string, dirs []string) error {
	keys := map[string]json.RawMessage{}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &keys); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	keys["workdirs"], _ = json.Marshal(dirs)
	return write(path, keys)
}

// Workdirs holds the working-directory choices and writes changes back to the config file.
type Workdirs struct {
	path      string
	workspace string

	mu   sync.Mutex
	dirs []string
}

func NewWorkdirs(path string, c Config) *Workdirs {
	return &Workdirs{path: path, workspace: c.Workspace, dirs: c.Workdirs}
}

func (w *Workdirs) Workspace() string { return w.workspace }

// List returns the saved choices, or every repository in the workspace if none were saved.
func (w *Workdirs) List() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dirs == nil {
		return Repos(w.workspace)
	}
	return append([]string{}, w.dirs...)
}

// Set saves dirs as the choices.
func (w *Workdirs) Set(dirs []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if dirs == nil {
		dirs = []string{}
	}
	if err := saveWorkdirs(w.path, dirs); err != nil {
		return err
	}
	w.dirs = append([]string{}, dirs...)
	return nil
}
