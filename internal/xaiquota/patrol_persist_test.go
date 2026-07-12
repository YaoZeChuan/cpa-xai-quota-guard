package xaiquota

import (
	"path/filepath"
	"testing"
)

func TestPatrolSnapshotPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLastPatrol(PatrolSnapshot{
		TotalProbed: 3,
		TotalAlive:  2,
		TotalErrors: 1,
		ByHTTP:      map[string]int{"200": 2, "0": 1},
		ByAction:    map[string]int{"alive": 2, "error": 1},
		RecentLog: []PatrolLogEntry{
			{TimeMS: 1, Action: "alive", HTTPCode: 200, Account: "a"},
			{TimeMS: 2, Action: "error", HTTPCode: 0, Account: "b"},
		},
		Scope: "all",
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.GetLastPatrol()
	if got == nil || got.TotalProbed != 3 || got.ByHTTP["200"] != 2 || len(got.RecentLog) != 2 {
		t.Fatalf("got=%+v", got)
	}
}