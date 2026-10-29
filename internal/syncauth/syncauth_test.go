package syncauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var errUnknown = errors.New("unknown key")

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// hub serves an echo handler behind the verifier, accepting the given keys
// (base64 → node; a node of 0 is revoked).
type hub struct {
	v    *Verifier
	h    http.Handler
	keys map[string]int64
	now  time.Time
}

func newHub(keys map[string]int64) *hub {
	x := &hub{keys: keys, now: time.UnixMilli(1_800_000_000_000)}
	x.v = NewVerifier(func(_ context.Context, pub string) (int64, error) {
		node, ok := x.keys[pub]
		if !ok || node == 0 {
			return 0, errUnknown
		}
		return node, nil
	})
	x.v.Now = func() time.Time { return x.now }
	x.h = x.v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, _ := Node(r.Context())
		body, _ := io.ReadAll(r.Body)
		w.Write([]byte(strings.Join([]string{string(rune('0' + node)), string(body)}, " ")))
	}))
	return x
}

// do sends req through the hub: the status and response body.
func (x *hub) do(req *http.Request) (int, string) {
	w := httptest.NewRecorder()
	x.h.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

func request(method, target, body string) *http.Request {
	return httptest.NewRequest(method, target, strings.NewReader(body))
}

// signed makes a request signed by s for body.
func signed(s *Signer, method, target, body string) *http.Request {
	req := request(method, target, body)
	s.Sign(req, []byte(body))
	return req
}

// resend copies req's headers onto a new request, which may differ.
func resend(req *http.Request, method, target, body string) *http.Request {
	out := request(method, target, body)
	out.Header = req.Header.Clone()
	return out
}

func TestValidSignaturePasses(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2})
	s := NewSigner(key)
	s.Now = func() time.Time { return x.now.Add(-30 * time.Second) }

	code, body := x.do(signed(s, "POST", "/sync/push", `{"changes":[]}`))
	if code != 200 || body != `2 {"changes":[]}` {
		t.Fatalf("%d %q", code, body)
	}
	code, body = x.do(signed(s, "GET", "/sync/pull?since=12&wait=30", ""))
	if code != 200 || body != "2 " {
		t.Fatalf("%d %q", code, body)
	}
	// The same request again is signed anew, not a replay.
	for range 2 {
		if code, _ := x.do(signed(s, "GET", "/sync/pull?since=12&wait=30", "")); code != 200 {
			t.Fatalf("repeated request: %d", code)
		}
	}
}

func TestTamperedRequestsFail(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2})
	s := NewSigner(key)
	s.Now = func() time.Time { return x.now }
	orig := signed(s, "POST", "/sync/push?x=1", "body")

	cases := map[string]*http.Request{
		"body":           resend(orig, "POST", "/sync/push?x=1", "bodx"),
		"path":           resend(orig, "POST", "/sync/lease?x=1", "body"),
		"query":          resend(orig, "POST", "/sync/push?x=2", "body"),
		"method":         resend(orig, "PUT", "/sync/push?x=1", "body"),
		"no query":       resend(orig, "POST", "/sync/push", "body"),
		"rewritten path": resend(orig, "POST", "/push?x=1", "body"),
	}
	ts := resend(orig, "POST", "/sync/push?x=1", "body")
	ts.Header.Set(HeaderTimestamp, "1800000000001")
	cases["timestamp"] = ts
	other := resend(orig, "POST", "/sync/push?x=1", "body")
	other.Header.Set(HeaderNode, PublicKey(newKey(t)))
	cases["other key"] = other
	for _, h := range []string{HeaderNode, HeaderTimestamp, HeaderSignature} {
		r := resend(orig, "POST", "/sync/push?x=1", "body")
		r.Header.Del(h)
		cases["no "+h] = r
	}
	garbled := resend(orig, "POST", "/sync/push?x=1", "body")
	garbled.Header.Set(HeaderSignature, "!!")
	cases["garbled signature"] = garbled

	for name, req := range cases {
		if code, _ := x.do(req); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := x.do(orig); code != 200 {
		t.Fatalf("original: %d", code)
	}
}

func TestClockSkew(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2})
	for _, c := range []struct {
		off  time.Duration
		code int
	}{
		{-61 * time.Second, 401}, {61 * time.Second, 401}, {-60 * time.Second, 200}, {59 * time.Second, 200},
		{-time.Hour, 401},
	} {
		s := NewSigner(key) // timestamps only go up within a signer
		s.Now = func() time.Time { return x.now.Add(c.off) }
		if code, _ := x.do(signed(s, "GET", "/sync/pull", "")); code != c.code {
			t.Errorf("off %v: %d, want %d", c.off, code, c.code)
		}
	}
}

func TestReplayFails(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2})
	s := NewSigner(key)
	s.Now = func() time.Time { return x.now }
	req := signed(s, "POST", "/sync/push", "body")
	if code, _ := x.do(resend(req, "POST", "/sync/push", "body")); code != 200 {
		t.Fatalf("first: %d", code)
	}
	x.now = x.now.Add(10 * time.Second)
	if code, _ := x.do(resend(req, "POST", "/sync/push", "body")); code != 401 {
		t.Fatalf("replay: %d", code)
	}
	// The same signature in another base64 spelling is still a replay: the
	// last character before "==" carries 4 unused bits (it is A, Q, g or w),
	// setting one leaves the bytes as they are.
	sig := req.Header.Get(HeaderSignature)
	respelled := sig[:len(sig)-3] + string(sig[len(sig)-3]+1) + "=="
	if !bytes.Equal(mustDecode(t, respelled), mustDecode(t, sig)) {
		t.Fatalf("%s and %s decode differently", sig, respelled)
	}
	alt := resend(req, "POST", "/sync/push", "body")
	alt.Header.Set(HeaderSignature, respelled)
	if code, _ := x.do(alt); code != 401 {
		t.Fatalf("respelled replay: %d", code)
	}
	// Once too old to pass the skew check, it is forgotten and refused anyway.
	x.now = x.now.Add(2 * MaxSkew)
	x.do(signed(s, "GET", "/sync/pull", "")) // prunes
	if _, ok := x.v.seen[string(mustDecode(t, sig))]; ok || len(x.v.seen) != 1 {
		t.Fatalf("%d signatures remembered", len(x.v.seen))
	}
	if code, _ := x.do(resend(req, "POST", "/sync/push", "body")); code != 401 {
		t.Fatalf("old replay: %d", code)
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestUnknownAndRevokedKeysFail(t *testing.T) {
	key, revoked := newKey(t), newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2, PublicKey(revoked): 0})
	for name, k := range map[string]ed25519.PrivateKey{"unknown": newKey(t), "revoked": revoked} {
		s := NewSigner(k)
		s.Now = func() time.Time { return x.now }
		if code, _ := x.do(signed(s, "GET", "/sync/pull", "")); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestBodyTooLarge(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 2})
	x.v.MaxBody = 4
	s := NewSigner(key)
	s.Now = func() time.Time { return x.now }
	if code, _ := x.do(signed(s, "POST", "/sync/push", "12345")); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", code)
	}
	if code, _ := x.do(signed(s, "POST", "/sync/push", "1234")); code != 200 {
		t.Fatalf("%d", code)
	}
}

// A real round trip: the client's request line is what the hub verifies.
func TestOverHTTP(t *testing.T) {
	key := newKey(t)
	x := newHub(map[string]int64{PublicKey(key): 7})
	x.v.Now = time.Now
	srv := httptest.NewServer(x.h)
	defer srv.Close()
	s := NewSigner(key)
	body := []byte(`{"a":"b c"}`)
	req, err := http.NewRequest("POST", srv.URL+"/sync/push?since=3&name=a%20b", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	s.Sign(req, body)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(got) != `7 {"a":"b c"}` {
		t.Fatalf("%d %q", resp.StatusCode, got)
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "sync_ed25519")
	key, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, %v", info.Mode(), err)
	}
	again, err := LoadOrCreateKey(path)
	if err != nil || !again.Equal(key) {
		t.Fatalf("reloaded key differs: %v", err)
	}

	os.Chmod(path, 0o644)
	if _, err := LoadOrCreateKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("readable key: %v", err)
	}
	os.WriteFile(path, []byte("junk"), 0o600)
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("junk key loaded")
	}
}
