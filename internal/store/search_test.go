package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestSearchTasks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)
	s.SetClock(func() time.Time { now = now.Add(time.Minute); return now })
	lanes, _ := s.Lanes(ctx)
	other, err := s.CreateProject(ctx, Project{Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	research, _ := s.CreateTask(ctx, Task{Title: "Research caching", Description: "Look at Redis", LaneID: lanes[0].ID})
	old, _ := s.CreateTask(ctx, Task{Title: "Old caching spike", LaneID: lanes[0].ID, ProjectID: other.ID})
	odd, _ := s.CreateTask(ctx, Task{Title: "100% done_ish", LaneID: lanes[0].ID})
	s.ArchiveTask(ctx, old.ID)

	ids := func(q string, limit int) []int64 {
		t.Helper()
		ts, err := s.SearchTasks(ctx, q, limit)
		if err != nil {
			t.Fatal(err)
		}
		out := []int64{}
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	eq := func(q string, got, want []int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%q = %v, want %v", q, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%q = %v, want %v", q, got, want)
			}
		}
	}
	// Other projects and archived tasks are found; tasks on a board come first.
	eq("CACHING", ids("CACHING", 20), []int64{research.ID, old.ID})
	// Every word must match, in the title or the description.
	eq("caching redis", ids("caching redis", 20), []int64{research.ID})
	eq("caching nope", ids("caching nope", 20), nil)
	// By id, with or without #.
	eq("#id", ids("#"+itoa(old.ID), 20), []int64{old.ID})
	// LIKE wildcards are literal.
	eq("%", ids("%", 20), []int64{odd.ID})
	eq("_", ids("e_i", 20), []int64{odd.ID})
	// Empty: on the board first, most recently updated first; limited.
	eq("", ids("", 2), []int64{odd.ID, research.ID})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
