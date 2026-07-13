package xaiquota

import (
	"runtime"
	"testing"
)

func TestResolvePatrolWorkersCapsByUserMax(t *testing.T) {
	w := resolvePatrolWorkers(2, 100)
	if w > 2 || w < 1 {
		t.Fatalf("workers=%d want in [1,2]", w)
	}
	w = resolvePatrolWorkers(0, 3)
	if w < 1 || w > 3 {
		t.Fatalf("default workers=%d invalid for 3 candidates", w)
	}
	w = resolvePatrolWorkers(32, 1)
	if w != 1 {
		t.Fatalf("workers=%d want 1 when only 1 candidate", w)
	}
	// With high max and many candidates, auto should be >= NumCPU on multi-core.
	if runtime.NumCPU() >= 2 {
		w = resolvePatrolWorkers(64, 1000)
		if w < runtime.NumCPU() {
			t.Fatalf("aggressive auto workers=%d < cpu=%d", w, runtime.NumCPU())
		}
		if w > 32 && w != 64 { // soft auto cap 32 unless user max forces lower
			// w is min(auto,max)=min(<=32,64) so <=32
			if w > 32 {
				t.Fatalf("workers=%d want <=32 soft auto cap", w)
			}
		}
	}
}
