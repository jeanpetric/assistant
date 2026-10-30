package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlackClient(t *testing.T) {
	dir := t.TempDir()
	empty, token := filepath.Join(dir, "empty"), filepath.Join(dir, "token")
	os.WriteFile(empty, []byte(" \n"), 0o600)
	os.WriteFile(token, []byte("xoxb-123\n"), 0o600)
	for _, path := range []string{"", filepath.Join(dir, "missing"), empty} {
		if c, err := slackClient(path); c != nil || err != nil {
			t.Errorf("%q: %v, %v; want Slack off", path, c, err)
		}
	}
	c, err := slackClient(token)
	if err != nil || c == nil || c.Token != "xoxb-123" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := slackClient(dir); err == nil {
		t.Fatal("a directory: want an error")
	}
}
