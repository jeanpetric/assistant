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
	if c != Defaults("/home/me") || c.Port != 6767 || c.LimitsRefreshSeconds != 120 {
		t.Fatalf("got %+v", c)
	}
	again, err := Load(path, "/home/me")
	if err != nil || again != c {
		t.Fatalf("reload: %+v %v", again, err)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"port": 7000, "mode": "plan", "limits_refresh_seconds": 0}`), 0o600)
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 || c.Mode != "plan" || c.Claude != "claude" || c.LimitsRefreshSeconds != 0 {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"bad.json": `{"port":`, "range.json": `{"port": 70000}`,
		"refresh.json": `{"limits_refresh_seconds": -1}`} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path, "/h"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
