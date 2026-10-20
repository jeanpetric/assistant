package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(path, "/home/me")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Defaults("/home/me")) || c.Port != 6767 || c.Workspace != "/home/me/workspace" {
		t.Fatalf("got %+v", c)
	}
	again, err := Load(path, "/home/me")
	if err != nil || !reflect.DeepEqual(again, c) {
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

func TestRepos(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"b/.git", "a/.git", "plain"} {
		os.MkdirAll(filepath.Join(root, d), 0o700)
	}
	os.MkdirAll(filepath.Join(root, "wt"), 0o700)
	os.WriteFile(filepath.Join(root, "wt", ".git"), []byte("gitdir: x"), 0o600) // linked worktree
	got := Repos(root)
	want := []string{filepath.Join(root, "a"), filepath.Join(root, "b")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if Repos(filepath.Join(root, "missing")) != nil {
		t.Fatal("missing root should list nothing")
	}
}

func TestWorkdirs(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	os.MkdirAll(filepath.Join(ws, "repo", ".git"), 0o700)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"port": 7000, "workspace": "`+ws+`"}`), 0o600)
	c, err := Load(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	w := NewWorkdirs(path, c)
	if got := w.List(); !reflect.DeepEqual(got, []string{filepath.Join(ws, "repo")}) {
		t.Fatalf("default list: %v", got)
	}

	if err := w.Set([]string{"/x", "/y"}); err != nil {
		t.Fatal(err)
	}
	if got := w.List(); !reflect.DeepEqual(got, []string{"/x", "/y"}) {
		t.Fatalf("after set: %v", got)
	}
	c, err = Load(path, dir)
	if err != nil || c.Port != 7000 || !reflect.DeepEqual(c.Workdirs, []string{"/x", "/y"}) {
		t.Fatalf("reload: %+v %v", c, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"db"`) {
		t.Fatalf("saving workdirs wrote keys the file did not have: %s", raw)
	}

	// An explicitly empty list stays empty instead of falling back to the workspace.
	if err := w.Set(nil); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(path, dir)
	if got := NewWorkdirs(path, c).List(); len(got) != 0 {
		t.Fatalf("empty list came back as %v", got)
	}
}
