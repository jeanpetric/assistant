package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestLeaseBlocksNeverOverlap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	exec(t, s, `INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (1500, 'old', 2, 0, 0, 0)`)
	type block struct{ from, to int64 }
	var got []block
	for _, node := range []int64{2, 3, 2} {
		from, to, err := s.LeaseBlock(ctx, "tasks", node)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, block{from, to})
	}
	want := []block{{2000, 2999}, {3000, 3999}, {4000, 4999}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("blocks %v, want %v", got, want)
	}
	// Other tables lease on their own; messages in bigger blocks.
	if from, to, err := s.LeaseBlock(ctx, "messages", 2); err != nil || from != 100000 || to != 199999 {
		t.Fatalf("messages block %d-%d, %v", from, to, err)
	}
	if _, _, err := s.LeaseBlock(ctx, "task_fields", 2); err == nil {
		t.Fatal("leased a block of a table without ids")
	}
	// Other nodes' blocks are not this node's.
	var blocks int
	s.db.QueryRow(`SELECT COUNT(*) FROM id_blocks`).Scan(&blocks)
	if blocks != 0 {
		t.Fatalf("%d id blocks", blocks)
	}
}

func TestHubLeasesItsOwnIDs(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, _, err := s.LeaseBlock(ctx, "tasks", 2); err != nil {
		t.Fatal(err)
	}
	// Going on from its highest id, the hub would run into node 2's block.
	s.SetHub(true)
	task, err := s.CreateTask(ctx, Task{Title: "on the hub"})
	if err != nil || task.ID != 2000 {
		t.Fatalf("task %+v, %v", task, err)
	}
	var node int64
	s.db.QueryRow(`SELECT node_id FROM id_leases WHERE start_id = 2000`).Scan(&node)
	if node != 1 {
		t.Fatalf("block 2000 leased to node %d", node)
	}
	if from, _, _ := s.LeaseBlock(ctx, "tasks", 3); from != 3000 {
		t.Fatalf("next block from %d", from)
	}
}

func TestApplyPushedLogsWhatChanged(t *testing.T) {
	ns := nodes(t, 3)
	hub, a, b := ns[0], ns[1], ns[2]
	ctx := context.Background()
	push := func(from *Store, node int64) {
		t.Helper()
		changes, err := from.ExportChanges(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := hub.ApplyPushed(ctx, node, changes); err != nil {
			t.Fatal(err)
		}
		from.AckOutbox(ctx, changes[len(changes)-1].Seq)
	}
	task, err := a.CreateTask(ctx, Task{Title: "from a"})
	if err != nil {
		t.Fatal(err)
	}
	push(a, 2)
	if _, err := hub.CreateTask(ctx, Task{Title: "from the hub"}); err != nil {
		t.Fatal(err)
	}
	pullTitles := func(node int64, since int64, limit int) (Pull, []string) {
		t.Helper()
		p, err := hub.PullChanges(ctx, since, limit, node)
		if err != nil {
			t.Fatal(err)
		}
		var titles []string
		for _, c := range p.Changes {
			if c.Table == "tasks" {
				var title string
				hub.db.QueryRow(`SELECT json_extract(?, '$.title')`, string(c.Row)).Scan(&title)
				titles = append(titles, title)
			}
		}
		return p, titles
	}
	if _, got := pullTitles(2, 0, 0); fmt.Sprint(got) != "[from the hub]" {
		t.Fatalf("a pulls %v", got)
	}
	p, got := pullTitles(3, 0, 0)
	if fmt.Sprint(got) != "[from a from the hub]" || p.More {
		t.Fatalf("b pulls %v (%+v)", got, p)
	}
	if again, _ := pullTitles(3, p.NextSeq, 0); len(again.Changes) != 0 || again.NextSeq != p.NextSeq {
		t.Fatalf("pull after the end: %+v", again)
	}

	// A push that loses changes nothing and is not logged.
	before, _ := hub.LatestSeq(ctx)
	exec(t, hub, fmt.Sprintf(`UPDATE tasks SET title = 'hub later' WHERE id = %d`, task.ID))
	exec(t, a, fmt.Sprintf(`UPDATE tasks SET title = 'a earlier' WHERE id = %d`, task.ID))
	hub.db.Exec(`UPDATE sync_rows SET changed_at_ms = changed_at_ms + 3600000 WHERE tbl = 'tasks' AND pk_json = ?`,
		fmt.Sprintf("[%d]", task.ID))
	push(a, 2)
	if after, _ := hub.LatestSeq(ctx); after != before+1 {
		t.Fatalf("log went from %d to %d", before, after)
	}
	if _, got := pullTitles(2, before, 0); fmt.Sprint(got) != "[hub later]" {
		t.Fatalf("a pulls %v", got)
	}

	// Pages.
	for i := range 3 {
		if _, err := b.CreateTask(ctx, Task{Title: fmt.Sprint("b", i)}); err != nil {
			t.Fatal(err)
		}
	}
	push(b, 3)
	latest, _ := hub.LatestSeq(ctx)
	p, _ = pullTitles(2, latest-3, 2)
	if !p.More || p.NextSeq != latest-1 || len(p.Changes) == 0 {
		t.Fatalf("first page %+v", p)
	}
	if p, _ = pullTitles(2, p.NextSeq, 2); p.More || p.NextSeq != latest {
		t.Fatalf("last page %+v", p)
	}
}

func TestSnapshotLeavesOutTheHubsOwn(t *testing.T) {
	s := openSynced(t)
	ctx := context.Background()
	s.SetHub(true)
	if _, err := s.AddSyncKey(ctx, "key", "laptop", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTask(ctx, Task{Title: "shared"}); err != nil {
		t.Fatal(err)
	}
	latest, _ := s.LatestSeq(ctx)
	path := filepath.Join(t.TempDir(), "snap.db")
	seq, err := s.Snapshot(ctx, path)
	if err != nil || seq != latest || seq == 0 {
		t.Fatalf("seq %d (latest %d), %v", seq, latest, err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tasks, hubs int
	db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE title = 'shared'`).Scan(&tasks)
	db.QueryRow(`SELECT (SELECT COUNT(*) FROM sync_keys) + (SELECT COUNT(*) FROM id_leases) +
		(SELECT COUNT(*) FROM id_blocks) + (SELECT COUNT(*) FROM sync_outbox)`).Scan(&hubs)
	if tasks != 1 || hubs != 0 {
		t.Fatalf("tasks %d, hub rows %d", tasks, hubs)
	}
	if _, err := s.Snapshot(ctx, path); err == nil {
		t.Fatal("snapshot over an existing file")
	}
}

func TestLeasesMigrationKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	n := len(migrations) - 1 // before id_leases
	for _, m := range migrations[:n] {
		if _, err := db.Exec(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		fmt.Sprintf(`PRAGMA user_version = %d`, n),
		`INSERT INTO tasks (id, title, lane_id, position, created_at, updated_at) VALUES (252, 'plan', 2, 0, 0, 0)`,
		`INSERT INTO sync_keys (pubkey, name, node_id, created_at) VALUES ('k', 'laptop', 2, 0)`,
		`INSERT INTO id_blocks (tbl, next_id, end_id) VALUES ('tasks', 1000, 1999)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if task, err := s.Task(ctx, 252); err != nil || task.Title != "plan" {
		t.Fatalf("task %+v, %v", task, err)
	}
	if node, err := s.SyncKeyNode(ctx, "k"); err != nil || node != 2 {
		t.Fatalf("key node %d, %v", node, err)
	}
	// The hub's leases start above its own block.
	if from, _, err := s.LeaseBlock(ctx, "tasks", 2); err != nil || from != 2000 {
		t.Fatalf("lease from %d, %v", from, err)
	}
}
