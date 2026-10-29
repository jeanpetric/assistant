package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SyncKey is a public key allowed to sync with this node as a hub.
type SyncKey struct {
	PubKey    string     `json:"pubkey"` // base64 Ed25519 public key
	Name      string     `json:"name"`
	NodeID    int64      `json:"node_id"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ErrKeyRevoked: the key is known but no longer accepted.
var ErrKeyRevoked = errors.New("sync key revoked")

// AddSyncKey allows pubkey to sync as nodeID.
func (s *Store) AddSyncKey(ctx context.Context, pubkey, name string, nodeID int64) (SyncKey, error) {
	if pubkey == "" || nodeID < 1 {
		return SyncKey{}, fmt.Errorf("sync key needs a public key and a node")
	}
	k := SyncKey{PubKey: pubkey, Name: name, NodeID: nodeID, CreatedAt: fromMillis(s.ts())}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_keys (pubkey, name, node_id, created_at) VALUES (?, ?, ?, ?)`,
		pubkey, name, nodeID, k.CreatedAt.UnixMilli())
	return k, err
}

// RevokeSyncKey stops accepting pubkey; revoking it again changes nothing.
func (s *Store) RevokeSyncKey(ctx context.Context, pubkey string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE sync_keys SET revoked_at = COALESCE(revoked_at, ?) WHERE pubkey = ?`,
		s.ts(), pubkey))
}

// SyncKeys lists all keys, revoked ones included, oldest first.
func (s *Store) SyncKeys(ctx context.Context) ([]SyncKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT pubkey, name, node_id, created_at, revoked_at FROM sync_keys ORDER BY created_at, pubkey`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []SyncKey
	for rows.Next() {
		var k SyncKey
		var created int64
		var revoked sql.NullInt64
		if err := rows.Scan(&k.PubKey, &k.Name, &k.NodeID, &created, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt, k.RevokedAt = fromMillis(created), fromNullMillis(revoked)
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// SyncKeyNode is the node pubkey syncs as: ErrNotFound for an unknown key,
// ErrKeyRevoked for a revoked one.
func (s *Store) SyncKeyNode(ctx context.Context, pubkey string) (int64, error) {
	var node int64
	var revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT node_id, revoked_at FROM sync_keys WHERE pubkey = ?`, pubkey).Scan(&node, &revoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNotFound
	case err != nil:
		return 0, err
	case revoked.Valid:
		return 0, ErrKeyRevoked
	}
	return node, nil
}
