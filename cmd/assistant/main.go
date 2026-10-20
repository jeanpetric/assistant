// Command assistant serves a local kanban board for tasks and AI agent sessions.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jeanpetric/assistant/internal/agent"
	"github.com/jeanpetric/assistant/internal/app"
	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/store"
)

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7777", "listen address (keep it on localhost: agents run with your permissions)")
	dbPath := flag.String("db", filepath.Join(home, ".assistant", "assistant.db"), "SQLite database file")
	claude := flag.String("claude", "claude", "claude CLI binary")
	claudeArgs := flag.String("claude-args", "", "extra arguments for every claude invocation, space separated (e.g. \"--model opus\")")
	workdir := flag.String("workdir", home, "default working directory for agent sessions")
	mode := flag.String("mode", "bypassPermissions", "default permission mode: "+strings.Join(agent.Modes, ", "))
	flag.Parse()

	if host, _, err := net.SplitHostPort(*addr); err != nil || !isLoopback(host) {
		log.Fatalf("-addr must be a loopback address, got %q", *addr)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := st.MarkInterrupted(ctx); err != nil {
		log.Fatal(err)
	}

	runner := agent.ClaudeRunner{Command: *claude, ExtraArgs: strings.Fields(*claudeArgs)}
	a := app.New(st, runner, *workdir, *mode)
	go a.RunScheduler(ctx, 20*time.Second)

	srv := &http.Server{Addr: *addr, Handler: server.New(a), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("assistant listening on http://%s (db %s)", *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	a.Shutdown()
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
