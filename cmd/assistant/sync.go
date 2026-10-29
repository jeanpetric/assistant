package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeanpetric/assistant/internal/config"
	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
	"github.com/jeanpetric/assistant/internal/syncclient"
)

const syncUsage = `usage: assistant [flags] sync <command>

On a node:
  init --server URL   make this node's key, set sync_server, print the public key
  seed                first node, with your data: back the database up, send all of it to the blank hub
  join                every other node, with an empty database: fill it from the hub

On the hub (sync_listen set):
  authorize PUBKEY --name NAME   accept a node's key, giving it the next node id
  revoke NAME                    stop accepting the key named NAME (or that public key)
  keys                           list the keys, revoked ones included

Then restart the app (make restart) to sync.
`

// runSync runs "assistant sync ...": setting a node or the hub up, before
// the app syncs.
func runSync(ctx context.Context, cfg config.Config, configPath string, args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, syncUsage)
		return errors.New("no command")
	}
	fs := flag.NewFlagSet("sync "+args[0], flag.ContinueOnError)
	fs.SetOutput(out)
	server := fs.String("server", "", "the hub, e.g. https://assistant.example.com (init)")
	name := fs.String("name", "", "the node's name, e.g. laptop (authorize)")
	pos, err := parseArgs(fs, args[1:])
	if err != nil {
		return err
	}
	want := func(n int, what string) error {
		if len(pos) != n {
			return fmt.Errorf("sync %s wants %s", args[0], what)
		}
		return nil
	}
	switch args[0] {
	case "init":
		if err := want(0, "--server URL only"); err != nil {
			return err
		}
		return syncInit(cfg, configPath, *server, out)
	case "authorize":
		if err := want(1, "a public key and --name"); err != nil {
			return err
		}
		return withHub(ctx, cfg, func(st *store.Store) error {
			if err := syncauth.CheckPublicKey(pos[0]); err != nil {
				return err
			}
			k, err := st.AuthorizeSyncKey(ctx, pos[0], *name)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "authorized %s as node %d\n", k.Name, k.NodeID)
			return nil
		})
	case "revoke":
		if err := want(1, "a key's name or public key"); err != nil {
			return err
		}
		return withHub(ctx, cfg, func(st *store.Store) error { return syncRevoke(ctx, st, pos[0], out) })
	case "keys":
		if err := want(0, "no arguments"); err != nil {
			return err
		}
		return withHub(ctx, cfg, func(st *store.Store) error { return syncKeys(ctx, st, out) })
	case "seed", "join":
		if err := want(0, "no arguments"); err != nil {
			return err
		}
		return withClient(ctx, cfg, func(st *store.Store, c *syncclient.Client) error {
			if args[0] == "join" {
				if err := c.Join(ctx); err != nil {
					return err
				}
				node, _ := st.NodeID(ctx)
				fmt.Fprintf(out, "joined %s as node %d; restart the app (make restart) to keep syncing\n", cfg.SyncServer, node)
				return nil
			}
			n, err := c.Seed(ctx, func() error {
				path, err := backup(ctx, st, cfg.DB)
				if err == nil {
					fmt.Fprintf(out, "backed up to %s\n", path)
				}
				return err
			})
			if err != nil {
				return err
			}
			node, _ := st.NodeID(ctx)
			fmt.Fprintf(out, "seeded %s with %d rows as node %d; restart the app (make restart) to keep syncing\n", cfg.SyncServer, n, node)
			return nil
		})
	}
	fmt.Fprint(out, syncUsage)
	return fmt.Errorf("unknown command %q", args[0])
}

// parseArgs parses fs's flags wherever they are among args, and returns
// the other arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func syncInit(cfg config.Config, configPath, server string, out io.Writer) error {
	if server == "" {
		return errors.New("sync init wants --server URL, e.g. https://assistant.example.com")
	}
	if cfg.SyncListen != "" {
		return errors.New("sync_listen is set: this node is the hub, it syncs with no other")
	}
	key, err := syncauth.LoadOrCreateKey(cfg.SyncKey)
	if err != nil {
		return fmt.Errorf("sync_key: %w", err)
	}
	if _, err := syncclient.New(nil, server, key); err != nil {
		return err
	}
	server = strings.TrimRight(server, "/")
	if err := config.Set(configPath, "sync_server", server); err != nil {
		return err
	}
	fmt.Fprintf(out, `sync_server set to %s in %s
This node's public key (%s):

  %s

On the hub: assistant sync authorize %s --name <this node>
Then here: assistant sync seed (the first node, with your data) or assistant sync join (an empty database).
`, server, configPath, cfg.SyncKey, syncauth.PublicKey(key), syncauth.PublicKey(key))
	return nil
}

func syncRevoke(ctx context.Context, st *store.Store, which string, out io.Writer) error {
	keys, err := st.SyncKeys(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, k := range keys {
		if k.RevokedAt != nil || k.Name != which && k.PubKey != which {
			continue
		}
		if err := st.RevokeSyncKey(ctx, k.PubKey); err != nil {
			return err
		}
		fmt.Fprintf(out, "revoked %s (node %d)\n", k.Name, k.NodeID)
		n++
	}
	if n == 0 {
		return fmt.Errorf("no key in use named %q", which)
	}
	return nil
}

func syncKeys(ctx context.Context, st *store.Store, out io.Writer) error {
	keys, err := st.SyncKeys(ctx)
	if err != nil {
		return err
	}
	for _, k := range keys {
		state := "since " + k.CreatedAt.Format("2006-01-02")
		if k.RevokedAt != nil {
			state = "revoked " + k.RevokedAt.Format("2006-01-02")
		}
		fmt.Fprintf(out, "node %d\t%s\t%s\t%s\n", k.NodeID, k.Name, state, k.PubKey)
	}
	if len(keys) == 0 {
		fmt.Fprintln(out, "no keys; add one with assistant sync authorize")
	}
	return nil
}

// withHub runs fn on the hub's database, which the running app may have
// open too.
func withHub(ctx context.Context, cfg config.Config, fn func(*store.Store) error) error {
	if cfg.SyncListen == "" {
		return errors.New("sync_listen is not set: keys are authorized on the hub")
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st)
}

func withClient(ctx context.Context, cfg config.Config, fn func(*store.Store, *syncclient.Client) error) error {
	if cfg.SyncServer == "" {
		return errors.New("sync_server is not set: run assistant sync init --server URL first")
	}
	key, err := syncauth.LoadOrCreateKey(cfg.SyncKey)
	if err != nil {
		return fmt.Errorf("sync_key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o700); err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := syncclient.New(st, cfg.SyncServer, key)
	if err != nil {
		return err
	}
	return fn(st, c)
}

// backup copies the database into backups/ next to it, as make backup
// does, and returns the copy's path.
func backup(ctx context.Context, st *store.Store, db string) (string, error) {
	dir := filepath.Join(filepath.Dir(db), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "assistant-"+time.Now().Format("20060102-150405")+".db")
	return path, st.Backup(ctx, path)
}
