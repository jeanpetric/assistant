// Package syncauth signs sync requests with a node's Ed25519 key and checks
// them on the hub.
//
// A request carries three headers: X-Node, the sender's base64 public key;
// X-Timestamp, Unix milliseconds; and X-Signature, the base64 signature of
//
//	method \n path?query \n timestamp \n hex(sha256(body))
//
// The path and query are signed exactly as sent on the request line, so a
// proxy in front of the hub must pass them through unchanged.
package syncauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	HeaderNode      = "X-Node"
	HeaderTimestamp = "X-Timestamp"
	HeaderSignature = "X-Signature"
)

// MaxSkew is how far a request's timestamp may be from the hub's clock.
const MaxSkew = 60 * time.Second

// LoadOrCreateKey reads the private key at path (PKCS#8 PEM), generating
// one there, readable by the owner only, if there is none. A key file others
// can read is refused.
func LoadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return createKey(path)
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: key file is accessible to others (%v); chmod 600 it", path, info.Mode().Perm())
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s: no PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: not an Ed25519 key", path)
	}
	return key, nil
}

func createKey(path string) (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// O_EXCL: never overwrite a key that appeared meanwhile.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der}); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return key, nil
}

// PublicKey is key's public half as base64, as in X-Node and sync_keys.
func PublicKey(key ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
}

func message(method, uri, ts string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(method + "\n" + uri + "\n" + ts + "\n" + hex.EncodeToString(sum[:]))
}

// Signer signs a node's requests.
type Signer struct {
	key ed25519.PrivateKey
	pub string
	Now func() time.Time

	mu   sync.Mutex
	last int64 // latest timestamp used
}

func NewSigner(key ed25519.PrivateKey) *Signer {
	return &Signer{key: key, pub: PublicKey(key), Now: time.Now}
}

// Sign sets req's auth headers for body, which must be what req sends. Each
// request gets a later timestamp than the one before, so two identical
// requests never share a signature and none is taken for a replay.
func (s *Signer) Sign(req *http.Request, body []byte) {
	s.mu.Lock()
	ts := s.Now().UnixMilli()
	if ts <= s.last {
		ts = s.last + 1
	}
	s.last = ts
	s.mu.Unlock()
	stamp := strconv.FormatInt(ts, 10)
	sig := ed25519.Sign(s.key, message(req.Method, req.URL.RequestURI(), stamp, body))
	req.Header.Set(HeaderNode, s.pub)
	req.Header.Set(HeaderTimestamp, stamp)
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(sig))
}

// Keys looks up a public key (base64): the node it belongs to, or an error
// for an unknown or revoked key.
type Keys func(ctx context.Context, pubkey string) (nodeID int64, err error)

// Verifier checks signed requests on the hub.
type Verifier struct {
	keys    Keys
	Now     func() time.Time
	MaxBody int64 // largest request body accepted

	mu   sync.Mutex
	seen map[string]time.Time // signatures accepted → when they may be forgotten
}

func NewVerifier(keys Keys) *Verifier {
	return &Verifier{keys: keys, Now: time.Now, MaxBody: 64 << 20, seen: map[string]time.Time{}}
}

type nodeKey struct{}

// Node is the node that signed the request handled with ctx.
func Node(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(nodeKey{}).(int64)
	return id, ok
}

// Middleware lets through only requests signed by a known, unrevoked key,
// stamped within MaxSkew of now and not seen before; others get 401. The
// handler finds the sender with Node.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node, status, err := v.verify(r)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeKey{}, node)))
	})
}

var errUnauthorized = errors.New("unauthorized")

// verify checks r and replaces its body with the bytes read.
func (v *Verifier) verify(r *http.Request) (int64, int, error) {
	pubB64, stamp, sigB64 := r.Header.Get(HeaderNode), r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature)
	pub, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	ts, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	now := v.Now()
	if d := now.Sub(time.UnixMilli(ts)); d > MaxSkew || d < -MaxSkew {
		return 0, http.StatusUnauthorized, fmt.Errorf("timestamp off by %v; check the clock", d.Round(time.Second))
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, v.MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return 0, http.StatusRequestEntityTooLarge, err
		}
		return 0, http.StatusBadRequest, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if !ed25519.Verify(pub, message(r.Method, r.RequestURI, stamp, body), sig) {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	// Canonical encodings: base64 lets the same bytes be written differently.
	node, err := v.keys(r.Context(), base64.StdEncoding.EncodeToString(pub))
	if err != nil {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	if !v.remember(string(sig), now) {
		return 0, http.StatusUnauthorized, errUnauthorized
	}
	return node, 0, nil
}

// remember records an accepted signature, false if it was seen already. A
// signature is kept until its timestamp could no longer pass the skew check
// (2·MaxSkew covers either side), so the cache stays small.
func (v *Verifier) remember(sig string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for s, until := range v.seen {
		if now.After(until) {
			delete(v.seen, s)
		}
	}
	if _, ok := v.seen[sig]; ok {
		return false
	}
	v.seen[sig] = now.Add(2 * MaxSkew)
	return true
}
