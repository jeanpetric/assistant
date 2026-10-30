package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// nodes opens n stores of one database, as joined nodes are: copies of the
// first, numbered 1 to n, each with its own id blocks and capture on.
func nodes(t *testing.T, n int) []*Store {
	t.Helper()
	dir := t.TempDir()
	first := filepath.Join(dir, "node1.db")
	s, err := Open(first)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var out []*Store
	for i := 1; i <= n; i++ {
		path := filepath.Join(dir, fmt.Sprintf("node%d.db", i))
		if i > 1 {
			copyFile(t, first, path)
		}
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		exec(t, s, fmt.Sprintf(`UPDATE sync_node SET node_id = %d`, i))
		for tbl := range idTables {
			if err := s.AddBlock(context.Background(), tbl, int64(i*1000), int64(i*1000+999)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.SetSyncEnabled(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// export takes everything in from's outbox and acknowledges it.
func export(t *testing.T, from *Store) []Change {
	t.Helper()
	ctx := context.Background()
	changes, err := from.ExportChanges(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) > 0 {
		if err := from.AckOutbox(ctx, changes[len(changes)-1].Seq); err != nil {
			t.Fatal(err)
		}
	}
	return changes
}

func apply(t *testing.T, to *Store, changes []Change) {
	t.Helper()
	if err := to.ApplyChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
}

// exchange passes every node's changes to every other until none are left.
func exchange(t *testing.T, ns ...*Store) {
	t.Helper()
	for round := 0; ; round++ {
		if round > 5 {
			t.Fatal("changes keep coming")
		}
		quiet := true
		for _, from := range ns {
			changes := export(t, from)
			quiet = quiet && len(changes) == 0
			for _, to := range ns {
				if to != from {
					apply(t, to, changes)
				}
			}
		}
		if quiet {
			return
		}
	}
}

// dump is the content of every synced table, a line per row.
func dump(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	tables := make([]string, 0, len(syncTables))
	for tbl := range syncTables {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	var b strings.Builder
	for _, tbl := range tables {
		var obj string
		err := s.tx(ctx, func(tx *sql.Tx) error {
			cols, err := tableColumns(ctx, tx, tbl)
			if err != nil {
				return err
			}
			pairs := make([]string, len(cols))
			for i, c := range cols {
				pairs[i] = fmt.Sprintf(`'%s', "%s"`, c, c)
			}
			obj = "json_object(" + strings.Join(pairs, ", ") + ")"
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := s.db.Query(`SELECT ` + obj + ` FROM ` + tbl + ` ORDER BY "` + strings.Join(syncTables[tbl], `", "`) + `"`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var row string
			if err := rows.Scan(&row); err != nil {
				t.Fatal(err)
			}
			b.WriteString(tbl + " " + row + "\n")
		}
		rows.Close()
	}
	return b.String()
}

// same fails unless the nodes hold the same synced data.
func same(t *testing.T, ns ...*Store) {
	t.Helper()
	want := dump(t, ns[0])
	for i, s := range ns[1:] {
		if got := dump(t, s); got != want {
			t.Fatalf("node %d differs from node 1:\n%s\nnode 1:\n%s", i+2, got, want)
		}
	}
}

func task(t *testing.T, s *Store, id int64) (Task, bool) {
	t.Helper()
	got, err := s.Task(context.Background(), id)
	if errors.Is(err, ErrNotFound) {
		return got, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return got, true
}

// tick lets the clock move on, so the next change is stamped later.
func tick() { time.Sleep(3 * time.Millisecond) }

func TestApplyOfflineChangesOnBothSides(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 3)
	a, b, c := ns[0], ns[1], ns[2]
	ta, err := a.CreateTask(ctx, Task{Title: "from a", Fields: map[string]string{"slack": "https://a"}})
	if err != nil {
		t.Fatal(err)
	}
	tb, err := b.CreateTask(ctx, Task{Title: "from b"})
	if err != nil {
		t.Fatal(err)
	}
	if ta.ID/1000 != 1 || tb.ID/1000 != 2 {
		t.Fatalf("ids %d %d are not from the nodes' blocks", ta.ID, tb.ID)
	}
	if err := b.UpdateLane(ctx, 2, "Later", ""); err != nil {
		t.Fatal(err)
	}
	sess, err := c.CreateSession(ctx, Session{Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "hi"}); err != nil {
		t.Fatal(err)
	}

	exchange(t, a, b, c)
	same(t, a, b, c)
	for _, s := range ns {
		got, ok := task(t, s, ta.ID)
		if !ok || got.Title != "from a" || got.Fields["slack"] != "https://a" {
			t.Errorf("task %d: %+v", ta.ID, got)
		}
		if _, ok := task(t, s, tb.ID); !ok {
			t.Errorf("no task %d", tb.ID)
		}
	}
	// Applied changes are not pushed back.
	for i, s := range ns {
		if got := outbox(t, s); len(got) != 0 {
			t.Errorf("node %d echoes %v", i+1, got)
		}
	}
}

// The row changed last wins as a whole, whatever order changes arrive in.
func TestApplyLaterChangeWins(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	x, err := a.CreateTask(ctx, Task{Title: "x", Description: "d"})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)

	x.Title = "a's title"
	if err := a.UpdateTask(ctx, x); err != nil {
		t.Fatal(err)
	}
	tick()
	x.Title, x.Description = "b's title", "b's description"
	if err := b.UpdateTask(ctx, x); err != nil {
		t.Fatal(err)
	}
	// b's change reaches a first, then a's older one reaches b.
	exchange(t, b, a)
	same(t, a, b)
	if got, _ := task(t, a, x.ID); got.Title != "b's title" || got.Description != "b's description" {
		t.Fatalf("task = %+v", got)
	}

	// Same time: the higher node wins.
	changes := export(t, a)
	if len(changes) != 0 {
		t.Fatalf("left over %v", changes)
	}
	at := func(node int64, title string) Change {
		return Change{Table: "lanes", Key: json.RawMessage(`[3]`), ChangedAtMs: 1 << 50, NodeID: node,
			Row: json.RawMessage(fmt.Sprintf(`{"id":3,"name":%q,"color":"","position":1}`, title))}
	}
	apply(t, a, []Change{at(2, "two"), at(1, "one")})
	apply(t, b, []Change{at(1, "one"), at(2, "two")})
	same(t, a, b)
	var name string
	a.db.QueryRow(`SELECT name FROM lanes WHERE id = 3`).Scan(&name)
	if name != "two" {
		t.Fatalf("lane 3 = %q", name)
	}
}

// A deleted task stays deleted, with everything under it, even when the
// other node changed it later or added to it.
func TestApplyDeleteBeatsEdit(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	x, err := a.CreateTask(ctx, Task{Title: "x", Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.CreateSession(ctx, Session{TaskID: x.ID, Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)

	if err := a.DeleteTask(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	tick()
	x.Title = "edited later"
	if err := b.UpdateTask(ctx, x); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "late"}); err != nil {
		t.Fatal(err)
	}
	late, err := b.CreateSession(ctx, Session{TaskID: x.ID, Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, a, b)
	same(t, a, b)
	for i, s := range ns {
		if _, ok := task(t, s, x.ID); ok {
			t.Errorf("node %d still has task %d", i+1, x.ID)
		}
		var n int
		s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions WHERE id IN (?, ?)) + (SELECT COUNT(*) FROM messages WHERE session_id = ?)`,
			sess.ID, late.ID, sess.ID).Scan(&n)
		if n != 0 {
			t.Errorf("node %d keeps %d of the task's sessions and messages", i+1, n)
		}
	}
	// A late edit of the deleted task brings nothing back.
	apply(t, a, []Change{{Table: "tasks", Key: json.RawMessage(fmt.Sprint("[", x.ID, "]")), ChangedAtMs: 1 << 50, NodeID: 2,
		Row: json.RawMessage(fmt.Sprintf(`{"id":%d,"title":"zombie","lane_id":2}`, x.ID))}})
	if _, ok := task(t, a, x.ID); ok {
		t.Fatal("deleted task came back")
	}
}

// Rows deleted along with another are tombstones on the receiving node too.
func TestApplyCascades(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	x, err := a.CreateTask(ctx, Task{Title: "x", Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.CreateSession(ctx, Session{TaskID: x.ID, Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := a.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	if err := a.DeleteTask(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	same(t, a, b)
	for _, r := range [][2]string{
		{"tasks", fmt.Sprintf("[%d]", x.ID)},
		{"task_fields", fmt.Sprintf(`[%d,"slack"]`, x.ID)},
		{"sessions", fmt.Sprintf("[%d]", sess.ID)},
		{"messages", fmt.Sprintf("[%d]", msg.ID)},
	} {
		if _, deleted := syncRow(t, b, r[0], r[1]); !deleted {
			t.Errorf("%s %s is not a tombstone on b", r[0], r[1])
		}
	}
}

// Applying changes again, or to the node they came from, changes nothing.
func TestApplyTwiceChangesNothing(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	x, err := a.CreateTask(ctx, Task{Title: "x", Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := a.CreateTask(ctx, Task{Title: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteTask(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	changes := export(t, a)
	apply(t, b, changes)
	before := dump(t, b)
	apply(t, b, changes)
	apply(t, a, changes)
	if got := dump(t, b); got != before {
		t.Fatalf("again:\n%s\nbefore:\n%s", got, before)
	}
	same(t, a, b)
	if _, ok := task(t, b, x.ID); !ok {
		t.Fatal("no task")
	}
	if got := outbox(t, a); len(got) != 0 {
		t.Fatalf("a records %v", got)
	}
}

// A row may come before the row it refers to.
func TestApplyChildBeforeParent(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	p, err := a.CreateProject(ctx, Project{Name: "p"})
	if err != nil {
		t.Fatal(err)
	}
	x, err := a.CreateTask(ctx, Task{Title: "x", ProjectID: p.ID, Fields: map[string]string{"slack": "https://x"}})
	if err != nil {
		t.Fatal(err)
	}
	sess, err := a.CreateSession(ctx, Session{TaskID: x.ID, Status: "idle", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddMessage(ctx, Message{SessionID: sess.ID, Role: "user", Kind: "text", Content: "hi"}); err != nil {
		t.Fatal(err)
	}
	changes := export(t, a)
	slices.Reverse(changes)
	order := func(tbl string) int {
		return slices.IndexFunc(changes, func(c Change) bool { return c.Table == tbl })
	}
	if !(order("messages") < order("tasks") && order("task_fields") < order("tasks") && order("tasks") < order("projects")) {
		t.Fatal("changes are not children first")
	}
	apply(t, b, changes)
	same(t, a, b)
}

// A task moved into a lane the other node deleted goes to the first lane.
func TestApplyMovesTasksOutOfDeletedLanes(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	lane, err := a.CreateLane(ctx, "short-lived", "")
	if err != nil {
		t.Fatal(err)
	}
	x, err := a.CreateTask(ctx, Task{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	if err := a.DeleteLane(ctx, lane.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.MoveTask(ctx, x.ID, lane.ID, 0); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	same(t, a, b)
	var first int64
	a.db.QueryRow(`SELECT id FROM lanes ORDER BY position, id LIMIT 1`).Scan(&first)
	if got, _ := task(t, a, x.ID); got.LaneID != first {
		t.Fatalf("task in lane %d, want %d", got.LaneID, first)
	}
}

// Skills made with the same name on two nodes both stay, one renamed.
func TestApplyRenamesClashingNames(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	if _, err := a.SaveSkill(ctx, Skill{Name: "Review", Body: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SaveSkill(ctx, Skill{Name: "review", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	same(t, a, b)
	skills, err := a.Skills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range skills {
		names = append(names, s.Name+"="+s.Body)
	}
	if !slices.Contains(names, "Review=a") || !slices.Contains(names, "review (2)=b") {
		t.Fatalf("skills %v", names)
	}
}

// Only synced tables take changes, and a bad change applies nothing.
func TestApplyRejectsLocalTables(t *testing.T) {
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	if _, err := a.CreateTask(context.Background(), Task{Title: "x"}); err != nil {
		t.Fatal(err)
	}
	changes := export(t, a)
	before := dump(t, b)
	bad := append(changes, Change{Table: "sync_node", Key: json.RawMessage(`[1]`), ChangedAtMs: 1, NodeID: 1,
		Row: json.RawMessage(`{"id":1,"node_id":1}`)})
	if err := b.ApplyChanges(context.Background(), bad); err == nil {
		t.Fatal("applied a change to sync_node")
	}
	if got := dump(t, b); got != before {
		t.Fatal("a failed batch left changes")
	}
	if id, _ := b.NodeID(context.Background()); id != 2 {
		t.Fatalf("node %d", id)
	}
}

// A row taken out and put back on one node is back on the other: only rows
// with ids stay deleted.
func TestApplyReAddedKey(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	x, err := a.CreateTask(ctx, Task{Title: "x", Fields: map[string]string{"slack": "https://1"}})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	x.Fields = map[string]string{"slack": ""}
	if err := a.UpdateTask(ctx, x); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	tick()
	x.Fields = map[string]string{"slack": "https://2"}
	if err := a.UpdateTask(ctx, x); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	same(t, a, b)
	if got, _ := task(t, b, x.ID); got.Fields["slack"] != "https://2" {
		t.Fatalf("fields %v", got.Fields)
	}
}

// Each row is exported once, with its latest content, and a limit covers
// that many outbox entries.
func TestExportChanges(t *testing.T) {
	ctx := context.Background()
	a := nodes(t, 1)[0]
	x, err := a.CreateTask(ctx, Task{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"y", "z"} {
		x.Title = title
		if err := a.UpdateTask(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	y, err := a.CreateTask(ctx, Task{Title: "y"})
	if err != nil {
		t.Fatal(err)
	}
	all, err := a.ExportChanges(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Table != "tasks" || string(all[0].Key) != fmt.Sprint("[", x.ID, "]") ||
		string(all[1].Key) != fmt.Sprint("[", y.ID, "]") {
		t.Fatalf("changes %+v", all)
	}
	var row struct{ Title string }
	if err := json.Unmarshal(all[0].Row, &row); err != nil || row.Title != "z" {
		t.Fatalf("row %s: %v", all[0].Row, err)
	}

	first, err := a.ExportChanges(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Seq >= all[0].Seq {
		t.Fatalf("limited %+v", first)
	}
	if err := a.AckOutbox(ctx, all[0].Seq); err != nil {
		t.Fatal(err)
	}
	rest, err := a.ExportChanges(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Seq != all[1].Seq {
		t.Fatalf("after ack %+v", rest)
	}
	if err := a.DeleteTask(ctx, y.ID); err != nil {
		t.Fatal(err)
	}
	rest, err = a.ExportChanges(ctx, all[1].Seq, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || !rest[0].Deleted || rest[0].Row != nil {
		t.Fatalf("delete %+v", rest)
	}
}

// Kinds added and deleted on one node are on every node after a sync, as are
// the tasks a deleted kind made kind task.
func TestApplyKinds(t *testing.T) {
	ctx := context.Background()
	ns := nodes(t, 2)
	a, b := ns[0], ns[1]
	tk, err := a.CreateTask(ctx, Task{Title: "talk", Kind: "discussion"})
	if err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	if err := a.AddKind(ctx, "bug"); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteKind(ctx, "discussion"); err != nil {
		t.Fatal(err)
	}
	exchange(t, a, b)
	same(t, a, b)
	want := []string{"task", "coding", "research", "reminder", "bug"}
	if kinds, err := b.Kinds(ctx); err != nil || !slices.Equal(kinds, want) {
		t.Fatalf("kinds on b = %v, %v", kinds, err)
	}
	if got, ok := task(t, b, tk.ID); !ok || got.Kind != "task" {
		t.Fatalf("task on b = %+v", got)
	}
}
