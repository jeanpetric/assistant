// Command assistant serves a local kanban board for tasks and AI agent sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/config"
	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/store"
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
	flag.StringVar(&f.Mode, "mode", "", "default permission mode: "+strings.Join(agent.Modes, ", ")+" (config: mode)")
	flag.StringVar(&f.TrelloKey, "trello-key", "", "Trello API key (config: trello_key)")
	flag.StringVar(&f.TrelloToken, "trello-token", "", "Trello API token (config: trello_token)")
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
		case "mode":
			cfg.Mode = f.Mode
		case "trello-key":
			cfg.TrelloKey = f.TrelloKey
		case "trello-token":
			cfg.TrelloToken = f.TrelloToken
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
	a := app.New(st, runner, cfg.Workdir, cfg.Mode)
	go a.RunScheduler(ctx, 5*time.Second)
	if cfg.TrelloKey != "" && cfg.TrelloToken != "" {
		a.Trello = &trello.Client{Key: cfg.TrelloKey, Token: cfg.TrelloToken}
		go a.RunTrelloSync(ctx, time.Minute)
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
}
