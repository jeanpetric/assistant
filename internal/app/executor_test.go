package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jeanpetric/assistant/internal/store"
)

// synced applies row as table's row key changed on node 2, as a sync pull would.
func synced(t *testing.T, s *store.Store, table string, id int64, row map[string]any) {
	t.Helper()
	b, _ := json.Marshal(row)
	err := s.ApplyChanges(context.Background(), []store.Change{{Table: table, Key: json.RawMessage(fmt.Sprintf("[%d]", id)),
		ChangedAtMs: time.Now().Add(time.Hour).UnixMilli(), NodeID: 2, Row: b}})
	if err != nil {
		t.Fatal(err)
	}
}

func taskRow(task store.Task, laneID, node int64) map[string]any {
	row := map[string]any{"id": task.ID, "title": task.Title, "description": task.Description, "kind": task.Kind,
		"project_id": task.ProjectID, "lane_id": laneID, "position": task.Position, "lane_changed_at": task.LaneChangedAt.UnixMilli(),
		"created_at": task.CreatedAt.UnixMilli(), "updated_at": task.UpdatedAt.UnixMilli(), "node_id": node}
	if task.DueAt != nil {
		row["due_at"] = task.DueAt.UnixMilli()
	}
	return row
}

// A schedule runs only on the node that made it: another node's is seen
// here, but neither runs nor has its last and next run changed.
func TestForeignScheduleNotRun(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 8, 59, 30, 0, time.Local)
	a.Now = func() time.Time { return now }
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Digest"})
	mine, _ := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "0 9 * * *", Action: store.ActionAgent, Prompt: "mine", Enabled: true})
	theirs, _ := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "0 9 * * *", Action: store.ActionAgent, Prompt: "theirs", Enabled: true})
	synced(t, a.Store, "schedules", theirs.ID, map[string]any{"id": theirs.ID, "task_id": task.ID, "cron": theirs.Cron,
		"action": theirs.Action, "prompt": theirs.Prompt, "mode": "", "enabled": 1, "next_run_at": theirs.NextRunAt.UnixMilli(), "node_id": 2})

	now = now.Add(2 * time.Minute)
	if err := a.RunDueSchedules(ctx); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if reqs := r.requests(); len(reqs) != 1 || reqs[0].Prompt != "mine" {
		t.Fatalf("reqs = %+v", reqs)
	}
	if got, _ := a.Store.Schedule(ctx, mine.ID); got.LastRunAt == nil || !got.NextRunAt.After(now) {
		t.Fatalf("mine after run = %+v", got)
	}
	got, _ := a.Store.Schedule(ctx, theirs.ID)
	if got.NodeID != 2 || got.LastRunAt != nil || !got.NextRunAt.Equal(*theirs.NextRunAt) {
		t.Fatalf("theirs = %+v", got)
	}
	if all, _ := a.Store.Schedules(ctx, task.ID); len(all) != 2 {
		t.Fatalf("schedules = %+v", all)
	}
}

// The node that made a task sends its due reminder; another node's task is
// reminded of there.
func TestForeignTaskNoDueReminder(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	due := time.Now().Add(-time.Minute)
	mine, _ := a.Store.CreateTask(ctx, store.Task{Title: "Mine", DueAt: &due})
	theirs, _ := a.Store.CreateTask(ctx, store.Task{Title: "Theirs", DueAt: &due})
	synced(t, a.Store, "tasks", theirs.ID, taskRow(theirs, theirs.LaneID, 2))

	if err := a.RunDueReminders(ctx); err != nil {
		t.Fatal(err)
	}
	n, _ := a.Store.Notifications(ctx, true)
	if len(n) != 1 || n[0].Message != "Due: Mine" || *n[0].TaskID != mine.ID {
		t.Fatalf("notifications = %+v", n)
	}
	if got, _ := a.Store.Task(ctx, theirs.ID); got.RemindedAt != nil {
		t.Fatalf("theirs reminded: %+v", got)
	}
}

// A hub is only the shared database: its scheduler runs no schedule,
// reminder or auto-archive. The same node off sync runs them all.
func TestHubSchedulerRunsNothing(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	now := time.Now()
	a.Store.SetClock(func() time.Time { return now.Add(-2 * time.Hour) })
	lanes := lanesByName(t, a)
	past := now.Add(-time.Minute)
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "Due", DueAt: &past})
	stale, _ := a.Store.CreateTask(ctx, store.Task{Title: "Old", LaneID: lanes["Done"]})
	a.Store.SetClock(time.Now)
	a.Store.SaveSettings(ctx, store.Settings{ArchiveLaneID: ptr(lanes["Done"]), ArchiveAfterMinutes: 60})
	x, _ := a.SaveSchedule(ctx, store.Schedule{TaskID: task.ID, Cron: "* * * * *", Action: store.ActionAgent, Prompt: "go", Enabled: true})
	a.Now = func() time.Time { return now.Add(2 * time.Minute) }

	// run gives RunScheduler a round: on a hub it must return at once.
	run := func() {
		t.Helper()
		sctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { a.RunScheduler(sctx, time.Hour); close(done) }()
		if a.SyncRole != RoleHub {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if got, _ := a.Store.Task(ctx, stale.ID); got.ArchivedAt != nil {
					break
				}
			}
			cancel()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("scheduler kept running")
		}
		cancel()
		a.Wait()
	}
	a.SyncRole = RoleHub
	run()
	if len(r.requests()) != 0 {
		t.Fatal("hub ran a schedule")
	}
	if got, _ := a.Store.Schedule(ctx, x.ID); got.LastRunAt != nil {
		t.Fatalf("hub marked the schedule run: %+v", got)
	}
	if n, _ := a.Store.Notifications(ctx, true); len(n) != 0 {
		t.Fatalf("hub notified: %+v", n)
	}
	if got, _ := a.Store.Task(ctx, stale.ID); got.ArchivedAt != nil {
		t.Fatal("hub archived")
	}

	a.SyncRole = RoleOff
	run()
	if len(r.requests()) != 1 {
		t.Fatalf("reqs = %+v", r.requests())
	}
	if got, _ := a.Store.Task(ctx, task.ID); got.RemindedAt == nil {
		t.Fatal("not reminded")
	}
	if got, _ := a.Store.Task(ctx, stale.ID); got.ArchivedAt == nil {
		t.Fatal("not archived")
	}
}

// A rule fires on the node where the task was moved: a lane change that
// comes in by sync fires nothing, a local move does.
func TestSyncedMoveFiresNoRule(t *testing.T) {
	a, r := newApp(t)
	ctx := context.Background()
	lanes := lanesByName(t, a)
	a.Store.SaveRule(ctx, store.Rule{Name: "Start", ToLaneID: lanes["In progress"], Enabled: true, Prompt: "work"})
	task, _ := a.Store.CreateTask(ctx, store.Task{Title: "T", LaneID: lanes["Todo"]})

	synced(t, a.Store, "tasks", task.ID, taskRow(task, lanes["In progress"], 1))
	a.Wait()
	if got, _ := a.Store.Task(ctx, task.ID); got.LaneID != lanes["In progress"] {
		t.Fatalf("lane = %d", got.LaneID)
	}
	if sessions, _ := a.Store.Sessions(ctx, task.ID, ""); len(sessions) != 0 || len(r.requests()) != 0 {
		t.Fatalf("synced move fired: %+v", sessions)
	}

	a.MoveTask(ctx, task.ID, lanes["Todo"], -1)
	if _, err := a.MoveTask(ctx, task.ID, lanes["In progress"], -1); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if reqs := r.requests(); len(reqs) != 1 || reqs[0].Prompt != "work" {
		t.Fatalf("local move: reqs = %+v", reqs)
	}
}

// The sync status gives the node's role, and a client's sync.
func TestSyncStatus(t *testing.T) {
	a, _ := newApp(t)
	ctx := context.Background()
	b := func(st SyncStatus) string { j, _ := json.Marshal(st); return string(j) }

	if st, err := a.SyncStatus(ctx); err != nil || b(st) != `{"role":"off"}` {
		t.Fatalf("off: %s, %v", b(st), err)
	}
	a.SyncRole = RoleHub
	if st, _ := a.SyncStatus(ctx); b(st) != `{"role":"hub"}` {
		t.Fatalf("hub: %s", b(st))
	}
	a.SyncRole, a.SyncServer = RoleClient, "https://hub.example"
	if st, _ := a.SyncStatus(ctx); st.ClientStatus == nil || st.Server != "https://hub.example" || st.Online || st.Error != errNotInSync {
		t.Fatalf("client not in sync: %s", b(st))
	}
	at := time.UnixMilli(1_700_000_000_000).UTC()
	a.SyncClient = func(context.Context) (ClientStatus, error) {
		return ClientStatus{Server: "https://hub.example", Online: true, Pending: 3, SyncedAt: &at}, nil
	}
	want := `{"role":"client","server":"https://hub.example","online":true,"pending":3,"synced_at":"2023-11-14T22:13:20Z"}`
	if st, _ := a.SyncStatus(ctx); b(st) != want {
		t.Fatalf("client: %s", b(st))
	}
}
