package main

import (
	"crypto/ed25519"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/config"
	"github.com/jeanpetric/assistant/internal/server"
	"github.com/jeanpetric/assistant/internal/syncclient"
)

// The deploy files and the runbook have to match the code: these tests read
// them as nginx, cloudflared, systemd and a shell would.

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// directive is one nginx directive; a block's directives are its children.
type directive struct {
	name     string
	args     []string
	children []directive
}

// parseNginx parses nginx config syntax: directives ending in ; or holding
// a { } block, # comments, quoted arguments.
func parseNginx(t *testing.T, src string) []directive {
	t.Helper()
	var toks []string
	for i := 0; i < len(src); {
		switch c := src[i]; {
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '{' || c == '}' || c == ';':
			toks = append(toks, string(c))
			i++
		case c == '"' || c == '\'':
			j := strings.IndexByte(src[i+1:], c)
			if j < 0 {
				t.Fatal("nginx: unterminated quote")
			}
			toks = append(toks, src[i+1:i+1+j])
			i += j + 2
		default:
			j := i
			for j < len(src) && !strings.ContainsRune(" \t\r\n{};#", rune(src[j])) {
				j++
			}
			toks = append(toks, src[i:j])
			i = j
		}
	}
	var block func(end string) []directive
	block = func(end string) []directive {
		var ds []directive
		for len(toks) > 0 {
			if toks[0] == end {
				toks = toks[1:]
				return ds
			}
			d := directive{name: toks[0]}
			toks = toks[1:]
			for len(toks) > 0 && toks[0] != ";" && toks[0] != "{" && toks[0] != "}" {
				d.args, toks = append(d.args, toks[0]), toks[1:]
			}
			if len(toks) == 0 || toks[0] == "}" {
				t.Fatalf("nginx: %s not ended", d.name)
			}
			if toks[0] == "{" {
				toks = toks[1:]
				d.children = block("}")
			} else {
				toks = toks[1:]
			}
			ds = append(ds, d)
		}
		if end != "" {
			t.Fatal("nginx: block not closed")
		}
		return ds
	}
	return block("")
}

func find(ds []directive, name string) []directive {
	var out []directive
	for _, d := range ds {
		if d.name == name {
			out = append(out, d)
		}
	}
	return out
}

// one is the single directive name in ds, with its arguments.
func one(t *testing.T, ds []directive, name string) directive {
	t.Helper()
	found := find(ds, name)
	if len(found) != 1 {
		t.Fatalf("nginx: want one %s, have %d", name, len(found))
	}
	return found[0]
}

// nginxSeconds reads an nginx time such as 90s, 2m or 90.
func nginxSeconds(t *testing.T, v string) time.Duration {
	t.Helper()
	unit := time.Second
	switch {
	case strings.HasSuffix(v, "ms"):
		unit, v = time.Millisecond, strings.TrimSuffix(v, "ms")
	case strings.HasSuffix(v, "s"):
		v = strings.TrimSuffix(v, "s")
	case strings.HasSuffix(v, "m"):
		unit, v = time.Minute, strings.TrimSuffix(v, "m")
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("nginx time %q", v)
	}
	return time.Duration(n) * unit
}

// nginxBytes reads an nginx size such as 64m.
func nginxBytes(t *testing.T, v string) int64 {
	t.Helper()
	mult := int64(1)
	switch strings.ToLower(v[len(v)-1:]) {
	case "k":
		mult, v = 1<<10, v[:len(v)-1]
	case "m":
		mult, v = 1<<20, v[:len(v)-1]
	case "g":
		mult, v = 1<<30, v[:len(v)-1]
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("nginx size %q", v)
	}
	return n * mult
}

// deploySetup is what the deploy files and the runbook agree on.
type deploySetup struct {
	hub        config.Config // the hub's config in the runbook
	configPath string        // where the runbook writes it
	nginxAddr  string        // nginx's listen address
	hostname   string        // nginx's server_name
	unit       string        // deploy/assistant.service
	runbook    string        // docs/sync.md
}

// hubConfig finds the runbook's heredoc writing the hub's config: its path
// and the config it holds over the defaults.
func hubConfig(t *testing.T, runbook string) (string, config.Config) {
	t.Helper()
	m := regexp.MustCompile(`(?s)tee (\S+config\.json) <<'EOF'\n(.*?)\nEOF\n`).FindStringSubmatch(runbook)
	if m == nil {
		t.Fatal("docs/sync.md: no heredoc writing the hub's config.json")
	}
	home := "/var/lib/assistant"
	cfg := config.Defaults(home)
	if err := json.Unmarshal([]byte(m[2]), &cfg); err != nil {
		t.Fatalf("docs/sync.md: hub config: %v", err)
	}
	return m[1], cfg
}

func loadDeploy(t *testing.T) (deploySetup, []directive) {
	t.Helper()
	var s deploySetup
	s.runbook = readRepoFile(t, "docs/sync.md")
	s.unit = readRepoFile(t, "deploy/assistant.service")
	s.configPath, s.hub = hubConfig(t, s.runbook)
	conf := parseNginx(t, readRepoFile(t, "deploy/nginx-assistant.conf"))
	srv := one(t, conf, "server")
	s.nginxAddr = one(t, srv.children, "listen").args[0]
	s.hostname = one(t, srv.children, "server_name").args[0]
	return s, conf
}

func isLoopback(t *testing.T, addr string) {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		t.Errorf("%q is not a loopback address", addr)
	}
}

func TestDeployNginx(t *testing.T) {
	s, conf := loadDeploy(t)
	if s.hub.SyncListen == "" || s.hub.SyncServer != "" {
		t.Fatalf("hub config: sync_listen %q, sync_server %q", s.hub.SyncListen, s.hub.SyncServer)
	}
	isLoopback(t, s.hub.SyncListen)
	isLoopback(t, s.nginxAddr)
	if _, port, _ := net.SplitHostPort(s.hub.SyncListen); port == strconv.Itoa(s.hub.Port) {
		t.Errorf("sync_listen %s shares the UI's port", s.hub.SyncListen)
	}

	srv := one(t, conf, "server")
	body := one(t, srv.children, "client_max_body_size").args[0]
	if n := nginxBytes(t, body); n < 16<<20 || n >= 100<<20 {
		t.Errorf("client_max_body_size %s: want room for a push, under Cloudflare's 100 MB", body)
	}

	locs := find(srv.children, "location")
	var syncLoc *directive
	for i, l := range locs {
		if len(l.args) == 1 && l.args[0] == "/sync/" {
			syncLoc = &locs[i]
			continue
		}
		// Everything else is 404.
		if r := find(l.children, "return"); len(r) != 1 || r[0].args[0] != "404" || len(l.children) != 1 {
			t.Errorf("location %v: want only return 404", l.args)
		}
	}
	if syncLoc == nil {
		t.Fatal("no location /sync/")
	}
	for _, d := range []string{"return", "proxy_pass", "rewrite"} {
		if len(find(srv.children, d)) > 0 {
			t.Errorf("%s outside the locations", d)
		}
	}
	hasRoot := false
	for _, l := range locs {
		hasRoot = hasRoot || len(l.args) == 1 && l.args[0] == "/"
	}
	if !hasRoot {
		t.Error("no location / returning 404")
	}

	sync := syncLoc.children
	// No URI after the address: the hub sees the path and query that were
	// signed.
	if got := one(t, sync, "proxy_pass").args; len(got) != 1 || got[0] != "http://"+s.hub.SyncListen {
		t.Errorf("proxy_pass %v: want http://%s with no path", got, s.hub.SyncListen)
	}
	for _, d := range []string{"rewrite", "proxy_hide_header", "proxy_pass_request_headers", "proxy_pass_request_body", "proxy_set_body", "location"} {
		if len(find(sync, d)) > 0 {
			t.Errorf("%s in /sync/", d)
		}
	}
	for _, h := range find(sync, "proxy_set_header") {
		switch strings.ToLower(h.args[0]) {
		case "x-node", "x-timestamp", "x-signature", "content-type", "content-length":
			t.Errorf("proxy_set_header %s: the signed request must pass as it came", h.args[0])
		}
	}
	if got := one(t, sync, "proxy_buffering").args; got[0] != "off" {
		t.Errorf("proxy_buffering %v", got)
	}
	longest := server.NewSyncHub(nil).MaxWait
	c, err := syncclient.New(nil, "https://"+s.hostname, ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	longest = max(longest, c.Wait)
	if got := nginxSeconds(t, one(t, sync, "proxy_read_timeout").args[0]); got <= longest+10*time.Second {
		t.Errorf("proxy_read_timeout %v: a pull waits up to %v", got, longest)
	}

	// limit_req on /sync/, keyed on the client Cloudflare saw: nginx sees
	// only cloudflared on 127.0.0.1.
	zone := one(t, conf, "limit_req_zone")
	if zone.args[0] != "$http_cf_connecting_ip" {
		t.Errorf("limit_req_zone key %s", zone.args[0])
	}
	var zoneName string
	for _, a := range zone.args {
		if v, ok := strings.CutPrefix(a, "zone="); ok {
			zoneName, _, _ = strings.Cut(v, ":")
		}
	}
	if lr := one(t, sync, "limit_req"); zoneName == "" || !strings.Contains(" "+strings.Join(lr.args, " ")+" ", " zone="+zoneName+" ") {
		t.Errorf("limit_req %v: want zone=%s", lr.args, zoneName)
	}
}

func TestDeployCloudflared(t *testing.T) {
	s, _ := loadDeploy(t)
	var rules [][2]string // hostname, service
	for _, line := range strings.Split(readRepoFile(t, "deploy/cloudflared-ingress.yml"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		item := strings.HasPrefix(line, "- ")
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":")
		if !ok {
			continue
		}
		if item {
			rules = append(rules, [2]string{})
		}
		if len(rules) == 0 {
			continue
		}
		switch strings.TrimSpace(key) {
		case "hostname":
			rules[len(rules)-1][0] = strings.TrimSpace(val)
		case "service":
			rules[len(rules)-1][1] = strings.TrimSpace(val)
		}
	}
	if len(rules) != 2 {
		t.Fatalf("ingress rules %v: want the hostname and the catch-all", rules)
	}
	if rules[0] != [2]string{s.hostname, "http://" + s.nginxAddr} {
		t.Errorf("ingress %v: want %s to http://%s (nginx)", rules[0], s.hostname, s.nginxAddr)
	}
	if rules[1] != [2]string{"", "http_status:404"} {
		t.Errorf("last ingress rule %v: want the catch-all http_status:404", rules[1])
	}
}

func TestDeploySystemdAndRunbook(t *testing.T) {
	s, _ := loadDeploy(t)
	m := regexp.MustCompile(`(?m)^ExecStart=(\S+) -config (\S+)$`).FindStringSubmatch(s.unit)
	if m == nil {
		t.Fatal("assistant.service: want ExecStart=<binary> -config <path>")
	}
	bin, cfgPath := m[1], m[2]
	if cfgPath != s.configPath {
		t.Errorf("assistant.service runs with %s, the runbook writes %s", cfgPath, s.configPath)
	}
	for _, want := range []string{"Restart=on-failure", "User=assistant", "ReadWritePaths=" + filepath.Dir(cfgPath)} {
		if !strings.Contains(s.unit, "\n"+want+"\n") {
			t.Errorf("assistant.service: no %s", want)
		}
	}
	if dir := filepath.Dir(cfgPath); filepath.Dir(s.hub.DB) != dir {
		t.Errorf("hub db %s is outside %s, the one directory it may write", s.hub.DB, dir)
	}
	if !strings.Contains(s.unit, `"sync_listen": "`+s.hub.SyncListen+`"`) {
		t.Errorf("assistant.service comment: want sync_listen %s", s.hub.SyncListen)
	}

	// The runbook names the same binary, files, addresses and hostname.
	_, nginxPort, _ := net.SplitHostPort(s.nginxAddr)
	for _, want := range []string{
		bin, cfgPath, s.hub.SyncListen, "127.0.0.1:" + nginxPort, "https://" + s.hostname,
		"deploy/assistant.service", "deploy/nginx-assistant.conf", "deploy/cloudflared-ingress.yml",
		"curl -i https://" + s.hostname + "/sync/hello", "make backup", "make start",
	} {
		if !strings.Contains(s.runbook, want) {
			t.Errorf("docs/sync.md: no %q", want)
		}
	}
	for _, f := range []string{"deploy/cloudflared-ingress.yml", "deploy/nginx-assistant.conf"} {
		if !strings.Contains(readRepoFile(t, f), s.hostname) {
			t.Errorf("%s: no %s", f, s.hostname)
		}
	}
}

// syncCommands reads "assistant [flags] sync <command> ..." lines from the
// runbook as a shell would split them: the global flags and the sync
// arguments.
func syncCommands(runbook string) (cmds [][2][]string) {
	for _, line := range strings.Split(runbook, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(strings.NewReplacer("`", " ", "(", " ", ")", " ").Replace(line))
	next:
		for i := range f {
			if filepath.Base(f[i]) != "assistant" {
				continue
			}
			var global []string
			for j := i + 1; j < len(f); j++ {
				switch {
				case f[j] == "sync":
					if j+1 < len(f) {
						cmds = append(cmds, [2][]string{global, f[j+1:]})
					}
					break next
				case strings.HasPrefix(f[j], "-") && j+1 < len(f):
					global = append(global, f[j])
					if !strings.Contains(f[j], "=") {
						j++
					}
				default:
					continue next
				}
			}
		}
	}
	return cmds
}

func TestRunbookSyncCommands(t *testing.T) {
	runbook := readRepoFile(t, "docs/sync.md")
	mainSrc := readRepoFile(t, "cmd/assistant/main.go")

	// The commands and their flags, as the usage lists them.
	type usage struct {
		flags      map[string]bool
		positional int
	}
	commands := map[string]usage{}
	for _, line := range strings.Split(syncUsage, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		words := strings.Fields(strings.SplitN(strings.TrimSpace(line), "  ", 2)[0])
		u := usage{flags: map[string]bool{}}
		for i := 1; i < len(words); i++ {
			if strings.HasPrefix(words[i], "--") {
				u.flags[words[i]] = true
				i++
			} else {
				u.positional++
			}
		}
		commands[words[0]] = u
	}
	for _, c := range []string{"init", "authorize", "revoke", "keys", "seed", "join"} {
		if _, ok := commands[c]; !ok {
			t.Fatalf("syncUsage has no %s: %v", c, commands)
		}
	}

	used := map[string]bool{}
	for _, c := range syncCommands(runbook) {
		global, args := c[0], c[1]
		for _, g := range global {
			name, _, _ := strings.Cut(strings.TrimLeft(g, "-"), "=")
			if !strings.Contains(mainSrc, `flag.String("`+name+`"`) && !regexp.MustCompile(`flag\.\w+Var\([^,]+, "`+regexp.QuoteMeta(name)+`"`).MatchString(mainSrc) {
				t.Errorf("assistant %s: no such flag", g)
			}
		}
		u, ok := commands[args[0]]
		if !ok {
			t.Errorf("assistant sync %s: no such command", strings.Join(args, " "))
			continue
		}
		used[args[0]] = true
		positional := 0
		for i := 1; i < len(args); i++ {
			if strings.HasPrefix(args[i], "-") {
				name, _, hasValue := strings.Cut(args[i], "=")
				if !u.flags["--"+strings.TrimLeft(name, "-")] {
					t.Errorf("assistant sync %s: %s is not a flag of %s", strings.Join(args, " "), name, args[0])
				}
				if !hasValue {
					i++
				}
				continue
			}
			positional++
		}
		if positional != u.positional {
			t.Errorf("assistant sync %s: %d arguments, %s takes %d", strings.Join(args, " "), positional, args[0], u.positional)
		}
	}
	// The runbook walks through every command.
	for c := range commands {
		if !used[c] {
			t.Errorf("docs/sync.md never runs assistant sync %s", c)
		}
	}
}

func TestSyncCommandsParse(t *testing.T) {
	got := syncCommands("x `./assistant sync init --server https://h` y\n" +
		"sudo -u assistant /usr/local/bin/assistant -config /c.json sync keys # comment\n" +
		"the assistant syncs\nassistant sync\n")
	if len(got) != 2 || strings.Join(got[0][1], " ") != "init --server https://h" ||
		strings.Join(got[1][0], " ") != "-config" || strings.Join(got[1][1], " ") != "keys" {
		t.Fatalf("got %q", got)
	}
}
