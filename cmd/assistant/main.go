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
)

func main() {
	home, _ := os.UserHomeDir()
	configPath := flag.String("config", filepath.Join(home, ".assistant", "config.json"), "JSON config file (created with defaults if missing)")
	var f config.Config
	flag.IntVar(&f.Port, "port", 0, "port on 127.0.0.1 (config: port, default 6767)")
	flag.StringVar(&f.DB, "db", "", "SQLite database file (config: db)")
	flag.StringVar(&f.Claude, "claude", "", "claude CLI binary (config: claude)")
	flag.StringVar(&f.ClaudeArgs, "claude-args", "", "extra arguments for every claude run, space separated, e.g. \"--model opus\" (config: claude_args)")
	flag.StringVar(&f.Workdir, "workdir", "", "default working directory for agent sessions (config: workdir)")
	flag.StringVar(&f.Mode, "mode", "", "default permission mode: "+strings.Join(agent.Modes, ", ")+" (config: mode)")
	flag.StringVar(&f.Model, "model", "", "default claude model, empty for the CLI default (config: model)")
	flag.StringVar(&f.Models, "models", "", "models offered in the UI, comma separated (config: models)")
	flag.StringVar(&f.Runner, "runner", "", "tmux: interactive claude per session in tmux; print: claude -p per turn (config: runner)")
	flag.StringVar(&f.TmuxSocket, "tmux-socket", "", "tmux -L socket name, empty for the default server (config: tmux_socket)")
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
		case "workdir":
			cfg.Workdir = f.Workdir
		case "mode":
			cfg.Mode = f.Mode
		case "model":
			cfg.Model = f.Model
		case "models":
			cfg.Models = f.Models
		case "runner":
			cfg.Runner = f.Runner
		case "tmux-socket":
			cfg.TmuxSocket = f.TmuxSocket
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

	var runner agent.Runner
	switch cfg.Runner {
	case "tmux":
		runner = agent.TmuxRunner{Claude: cfg.Claude, ExtraArgs: strings.Fields(cfg.ClaudeArgs),
			Socket: cfg.TmuxSocket, StateDir: filepath.Join(filepath.Dir(cfg.DB), "agents")}
	case "print":
		runner = agent.ClaudeRunner{Command: cfg.Claude, ExtraArgs: strings.Fields(cfg.ClaudeArgs)}
	default:
		log.Fatalf("runner must be tmux or print, got %q", cfg.Runner)
	}
	a := app.New(st, runner, cfg.Workdir, cfg.Mode)
	a.DefaultModel = cfg.Model
	for _, m := range strings.Split(cfg.Models, ",") {
		if m = strings.TrimSpace(m); m != "" {
			a.Models = append(a.Models, m)
		}
	}
	go a.RunScheduler(ctx, 20*time.Second)

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
