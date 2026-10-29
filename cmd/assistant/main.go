// Command assistant serves a local kanban board for tasks and AI agent sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/config"
	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/store"
	"github.com/jeanpetric/assistant/internal/syncauth"
	"github.com/jeanpetric/assistant/internal/syncclient"
	"github.com/jeanpetric/assistant/internal/trello"
)

func main() {
	home, _ := os.UserHomeDir()
	configPath := flag.String("config", filepath.Join(home, ".assistant", "config.json"), "JSON config file (created with defaults if missing)")
	var f config.Config
	flag.IntVar(&f.Port, "port", 0, "port on 127.0.0.1 (config: port, default 6767)")
	flag.StringVar(&f.DB, "db", "", "SQLite database file (config: db)")
	flag.StringVar(&f.Claude, "claude", "", "claude CLI binary (config: claude)")
	flag.StringVar(&f.ClaudeArgs, "claude-args", "", "extra arguments for every claude run, space separated, e.g. \"--model opus\" (config: claude_args)")
	flag.StringVar(&f.Gemini, "gemini", "", "gemini CLI binary (config: gemini)")
	flag.StringVar(&f.GeminiArgs, "gemini-args", "", "extra arguments for every gemini run, space separated (config: gemini_args)")
	flag.StringVar(&f.Workdir, "workdir", "", "default working directory for agent sessions (config: workdir)")
	flag.StringVar(&f.Agent, "agent", "", "default agent for new sessions: "+strings.Join(agent.Agents, ", ")+" (config: agent)")
	flag.StringVar(&f.Mode, "mode", "", "default permission mode: "+strings.Join(agent.Modes, ", ")+" (config: mode)")
	flag.StringVar(&f.TrelloKey, "trello-key", "", "Trello API key (config: trello_key)")
	flag.StringVar(&f.TrelloToken, "trello-token", "", "Trello API token (config: trello_token)")
	flag.StringVar(&f.SyncListen, "sync-listen", "", "address to serve /sync/* on as the sync hub, e.g. 127.0.0.1:6768; empty: not a hub (config: sync_listen)")
	flag.StringVar(&f.SyncServer, "sync-server", "", "sync hub to keep this node in sync with, e.g. https://assistant.example.com; empty: no sync (config: sync_server)")
	flag.StringVar(&f.SyncKey, "sync-key", "", "Ed25519 key signing sync requests, created if missing (config: sync_key)")
	flag.Parse()

	cfg, err := config.Load(*configPath, home)
	if err != nil {
		log.Fatal(err)
	}
	// Flags given on the command line win over the config file.
	flag.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "port":
			cfg.Port = f.Port
		case "db":
			cfg.DB = f.DB
		case "claude":
			cfg.Claude = f.Claude
		case "claude-args":
			cfg.ClaudeArgs = f.ClaudeArgs
		case "gemini":
			cfg.Gemini = f.Gemini
		case "gemini-args":
			cfg.GeminiArgs = f.GeminiArgs
		case "workdir":
			cfg.Workdir = f.Workdir
		case "agent":
			cfg.Agent = f.Agent
		case "mode":
			cfg.Mode = f.Mode
		case "trello-key":
			cfg.TrelloKey = f.TrelloKey
		case "trello-token":
			cfg.TrelloToken = f.TrelloToken
		case "sync-listen":
			cfg.SyncListen = f.SyncListen
		case "sync-server":
			cfg.SyncServer = f.SyncServer
		case "sync-key":
			cfg.SyncKey = f.SyncKey
		}
	})
	// Always loopback: agents run with your permissions.
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)

	if err := os.MkdirAll(filepath.Dir(cfg.DB), 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := st.MarkInterrupted(ctx); err != nil {
		log.Fatal(err)
	}

	runner := agent.Runners{
		agent.Claude: agent.ClaudeRunner{Command: cfg.Claude, ExtraArgs: strings.Fields(cfg.ClaudeArgs)},
		agent.Gemini: agent.GeminiRunner{Command: cfg.Gemini, ExtraArgs: strings.Fields(cfg.GeminiArgs)},
	}
	if cfg.Agent != "" && !slices.Contains(agent.Agents, cfg.Agent) {
		log.Fatalf("unknown agent %q: want one of %s", cfg.Agent, strings.Join(agent.Agents, ", "))
	}
	a := app.New(st, runner, cfg.Workdir, cfg.Mode)
	a.DefaultAgent = cfg.Agent
	go a.RunScheduler(ctx, 5*time.Second)
	if cfg.TrelloKey != "" && cfg.TrelloToken != "" {
		a.Trello = &trello.Client{Key: cfg.TrelloKey, Token: cfg.TrelloToken}
		go a.RunTrelloSync(ctx, time.Minute)
	}

	if cfg.SyncListen != "" && cfg.SyncServer != "" {
		log.Fatal("sync_listen and sync_server both set: a node is the hub or syncs with one")
	}
	if cfg.SyncListen != "" {
		if err := serveSync(ctx, st, cfg.SyncListen); err != nil {
			log.Fatal(err)
		}
	}
	syncDone := make(chan struct{})
	if cfg.SyncServer != "" {
		c, err := syncClient(ctx, st, cfg.SyncServer, cfg.SyncKey)
		if err != nil {
			log.Fatal(err)
		}
		go func() {
			defer close(syncDone)
			c.Run(ctx)
		}()
	} else {
		close(syncDone)
	}

	srv := &http.Server{Addr: addr, Handler: server.New(a), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("assistant listening on http://%s (db %s, config %s)", addr, cfg.DB, *configPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	a.Shutdown()
	<-syncDone
}

// syncClient keeps this node in sync with the hub at server: its changes are
// logged to be pushed, and its new ids come only from blocks the hub leased
// it, so they never clash with another node's.
func syncClient(ctx context.Context, st *store.Store, server, keyPath string) (*syncclient.Client, error) {
	key, err := syncauth.LoadOrCreateKey(keyPath)
	if err != nil {
		return nil, fmt.Errorf("sync_key: %w", err)
	}
	c, err := syncclient.New(st, server, key)
	if err != nil {
		return nil, err
	}
	if err := st.SetSyncEnabled(ctx, true); err != nil {
		return nil, err
	}
	st.SetLeasedIDs(true)
	log.Printf("syncing with %s as %s", server, syncauth.PublicKey(key))
	return c, nil
}

// serveSync makes this node the sync hub on addr: its changes are logged for
// the other nodes, and it takes its new ids from blocks it leases itself.
func serveSync(ctx context.Context, st *store.Store, addr string) error {
	if err := st.SetSyncEnabled(ctx, true); err != nil {
		return err
	}
	st.SetHub(true)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sync_listen: %w", err)
	}
	v := syncauth.NewVerifier(st.SyncKeyNode)
	go func() {
		if err := server.ServeSync(ctx, ln, server.NewSyncHub(st).Handler(v)); err != nil {
			log.Printf("sync: %v", err)
		}
	}()
	log.Printf("sync hub listening on http://%s", addr)
	return nil
}
