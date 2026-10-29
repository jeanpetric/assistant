package server

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeanpetric/assistant/internal/trello"
)

type fakeTrello struct{ cards map[string][]trello.Card }

func (f fakeTrello) OpenCards(_ context.Context, board string) ([]trello.Card, error) {
	return f.cards[board], nil
}
func (fakeTrello) SetClosed(context.Context, string, bool) error { return nil }

type trelloBoardResp struct {
	ID         int64  `json:"id"`
	Board      string `json:"board"`
	ProjectID  *int64 `json:"project_id"`
	DoneLaneID *int64 `json:"done_lane_id"`
	LastSyncAt any    `json:"last_sync_at"`
	Error      string `json:"error"`
}

type trelloResp struct {
	Configured bool              `json:"configured"`
	Boards     []trelloBoardResp `json:"boards"`
}

func TestTrelloBoardsAndSync(t *testing.T) {
	c := newClient(t)
	var got trelloResp
	c.do("GET", "/api/trello", nil, 200, &got)
	if got.Configured || got.Boards == nil || len(got.Boards) != 0 {
		t.Fatalf("initial = %+v", got)
	}
	c.do("POST", "/api/trello/boards", map[string]any{"board": " "}, 400, nil)
	c.do("POST", "/api/trello/boards", map[string]any{"board": "b", "done_lane_id": 999}, 400, nil)
	c.do("POST", "/api/trello/boards", map[string]any{"board": "b", "project_id": 999}, 400, nil)
	var a, b trelloBoardResp
	c.do("POST", "/api/trello/boards", map[string]any{"board": " https://trello.com/b/AbC/x ", "project_id": 1, "done_lane_id": 5}, 201, &a)
	if a.Board != "https://trello.com/b/AbC/x" || *a.ProjectID != 1 || *a.DoneLaneID != 5 {
		t.Fatalf("added = %+v", a)
	}
	// The same board can't be synced twice, by URL or short link.
	c.do("POST", "/api/trello/boards", map[string]any{"board": "AbC"}, 400, nil)
	c.do("POST", "/api/trello/boards", map[string]any{"board": "XyZ"}, 201, &b)
	c.do("PUT", "/api/trello/boards/"+itoa(b.ID), map[string]any{"board": "AbC"}, 400, nil)
	c.do("PUT", "/api/trello/boards/99", map[string]any{"board": "Other"}, 404, nil)
	c.do("PUT", "/api/trello/boards/"+itoa(b.ID), map[string]any{"board": "https://trello.com/b/XyZ/home", "done_lane_id": 4}, 200, &b)
	if b.Board != "https://trello.com/b/XyZ/home" || b.ProjectID != nil || *b.DoneLaneID != 4 {
		t.Fatalf("updated = %+v", b)
	}

	c.app.Trello = fakeTrello{cards: map[string][]trello.Card{
		"https://trello.com/b/AbC/x":    {{ID: "c1", Name: "From work"}},
		"https://trello.com/b/XyZ/home": {{ID: "c2", Name: "From home"}},
	}}
	c.do("POST", "/api/trello/sync", nil, 200, &got)
	if !got.Configured || len(got.Boards) != 2 || got.Boards[0].LastSyncAt == nil || got.Boards[1].Error != "" {
		t.Fatalf("after sync = %+v", got)
	}
	var tasks []struct{ Title string }
	c.do("GET", "/api/tasks?q=From", nil, 200, &tasks)
	if len(tasks) != 2 {
		t.Fatalf("tasks = %+v", tasks)
	}

	c.do("DELETE", "/api/trello/boards/"+itoa(a.ID), nil, 204, nil)
	c.do("DELETE", "/api/trello/boards/"+itoa(a.ID), nil, 404, nil)
	c.do("GET", "/api/trello", nil, 200, &got)
	if len(got.Boards) != 1 || got.Boards[0].ID != b.ID {
		t.Fatalf("after remove = %+v", got)
	}
}

func TestMCPTrelloBoards(t *testing.T) {
	c := newClient(t)
	text, isErr := c.tool("add_trello_board", map[string]any{"board": "https://trello.com/b/AbC/x", "project_id": 1, "lane_id": 2})
	if isErr || !strings.Contains(text, `"board":"https://trello.com/b/AbC/x"`) || !strings.Contains(text, `"lane_id":2`) {
		t.Fatalf("add: %s", text)
	}
	if text, isErr = c.tool("add_trello_board", map[string]any{"board": "AbC"}); !isErr || !strings.Contains(text, "already synced") {
		t.Fatalf("add twice: %s", text)
	}
	if text, isErr = c.tool("update_trello_board", map[string]any{"trello_board_id": 1, "board": "AbC", "done_lane_id": 5}); isErr ||
		!strings.Contains(text, `"done_lane_id":5`) || !strings.Contains(text, `"lane_id":null`) {
		t.Fatalf("update: %s", text)
	}
	if text, isErr = c.tool("list_trello_boards", map[string]any{}); isErr || !strings.Contains(text, `"configured":false`) || !strings.Contains(text, `"board":"AbC"`) {
		t.Fatalf("list: %s", text)
	}
	if text, isErr = c.tool("remove_trello_board", map[string]any{"trello_board_id": 1}); isErr {
		t.Fatalf("remove: %s", text)
	}
	if text, _ = c.tool("list_trello_boards", map[string]any{}); !strings.Contains(text, `"boards":[]`) {
		t.Fatalf("list after remove: %s", text)
	}
}

// The Trello panel lists each board with its selects and a Remove button, saves a
// changed board, and adds a new one. Runs under node with a fake DOM.
func TestTrelloPanel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(js, []byte("// Trello sync: boards whose"))
	end := bytes.Index(js, []byte("function ruleDialog("))
	if start < 0 || end < start {
		t.Fatal("app.js lacks trelloPanel")
	}
	src := filepath.Join(t.TempDir(), "trello.js")
	if err := os.WriteFile(src, js[start:end], 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
const fail = msg => { console.log(msg); process.exitCode = 1; };
function h(tag, attrs, ...children) {
  const on = {};
  const el = { tag, attrs: attrs || {}, value: (attrs || {}).value ?? '', children: children.flat(Infinity).filter(c => c != null && c !== false),
    addEventListener(ev, fn) { on[ev] = fn; }, fire(ev, e) { return on[ev](e); } };
  for (const [k, v] of Object.entries(el.attrs)) if (k.startsWith('on')) on[k.slice(2)] = v;
  return el;
}
const select = (options, value) => h('select', { value: String(value) }, options.map(([v, l]) => h('option', { value: v }, l)));
const find = (el, pred, out = []) => { if (el && typeof el === 'object') { if (pred(el)) out.push(el); (el.children || []).forEach(c => find(c, pred, out)); } return out; };
const text = el => typeof el === 'object' ? (el.children || []).map(text).join('') : String(el);
const calls = [];
const api = async (method, path, body) => { calls.push([method, path, body]); return null; };
const toast = () => {};
const fmtTime = () => 'now';
globalThis.confirm = () => true;
let rerendered = 0;
const panel = eval(require('fs').readFileSync(process.argv.at(-1), 'utf8') + '; trelloPanel');
const b = { projects: [{ id: 1, name: 'Assistant' }, { id: 2, name: 'Private' }], all_lanes: [{ id: 2, name: 'Todo' }, { id: 5, name: 'Done' }] };
const trello = { configured: true, boards: [
  { id: 1, board: 'https://trello.com/b/StAn/stanista', project_id: 2, lane_id: 2, done_lane_id: 5, last_sync_at: 'x', error: '' },
  { id: 2, board: 'https://trello.com/b/Home/home', project_id: 2, lane_id: null, done_lane_id: null, last_sync_at: null, error: 'boom' },
] };
(async () => {
  const p = panel(b, trello, () => rerendered++);
  const removes = find(p, e => e.tag === 'button' && text(e) === 'Remove');
  if (removes.length !== 2) fail('want a Remove per board, got ' + removes.length);
  if (!find(p, e => e.tag === 'input' && e.value === 'https://trello.com/b/StAn/stanista').length) fail('stanista missing');
  if (!text(p).includes('Last sync failed: boom')) fail('missing error of the second board');
  const selects = find(p, e => e.tag === 'select');
  if (selects.length !== 9 || selects[0].value !== '2' || selects[2].value !== '5' || selects[5].value !== '') fail('selects: ' + selects.map(s => s.value));
  selects[2].value = '';
  await selects[2].fire('change');
  const put = JSON.stringify(calls.at(-1));
  if (put !== JSON.stringify(['PUT', '/api/trello/boards/1', { board: 'https://trello.com/b/StAn/stanista', project_id: 2, lane_id: 2, done_lane_id: null }])) fail('save: ' + put);
  await removes[1].fire('click');
  if (JSON.stringify(calls.at(-1)) !== JSON.stringify(['DELETE', '/api/trello/boards/2', undefined]) || rerendered !== 1) fail('remove: ' + JSON.stringify(calls.at(-1)));
  const inputs = find(p, e => e.tag === 'input');
  const add = inputs.at(-1);
  add.value = 'https://trello.com/b/New/x';
  selects[6].value = '1';
  await find(p, e => e.tag === 'button' && text(e) === 'Add')[0].fire('click');
  if (JSON.stringify(calls.at(-1)) !== JSON.stringify(['POST', '/api/trello/boards', { board: 'https://trello.com/b/New/x', project_id: 1, lane_id: null, done_lane_id: null }])) fail('add: ' + JSON.stringify(calls.at(-1)));
})();
`
	out, err := exec.Command(node, "-e", script, "--", src).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("trelloPanel: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
