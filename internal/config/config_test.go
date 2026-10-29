package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if c != Defaults("/home/me") || c.Port != 6767 {
		t.Fatalf("got %+v", c)
	}
	again, err := Load(path, "/home/me")
	if err != nil || again != c {
		t.Fatalf("reload: %+v %v", again, err)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"port": 7000, "mode": "plan"}`), 0o600)
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 || c.Mode != "plan" || c.Claude != "claude" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"bad.json": `{"port":`, "range.json": `{"port": 70000}`} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path, "/h"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestLoadSyncServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"sync_server": "https://assistant.example.com"}`), 0o600)
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if c.SyncServer != "https://assistant.example.com" || c.SyncKey != "/home/me/.assistant/sync_ed25519" || c.SyncListen != "" {
		t.Fatalf("got %+v", c)
	}
}

func TestSetKeepsTheOtherSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"port": 7000, "trello_key": "k"}`), 0o600)
	if err := Set(path, "sync_server", "https://hub.example.com"); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 || c.TrelloKey != "k" || c.SyncServer != "https://hub.example.com" || c.Mode != "bypassPermissions" {
		t.Fatalf("got %+v", c)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
}
