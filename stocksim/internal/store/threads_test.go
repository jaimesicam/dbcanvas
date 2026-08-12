package store

import "testing"

// The thread count is the one configuration value that reaches both halves of
// this app — the agents' concurrency and the pool's connection ceiling — so a
// mistake here is either a pool that starves the workers it was raised for, or
// a mistyped number opening hundreds of connections to someone's database.

func TestClampThreads(t *testing.T) {
	for in, want := range map[int]int{
		0:              DefaultThreads,
		-1:             DefaultThreads,
		1:              1,
		8:              8,
		MaxThreads:     MaxThreads,
		MaxThreads + 1: MaxThreads,
		100_000:        MaxThreads,
	} {
		if got := ClampThreads(in); got != want {
			t.Errorf("ClampThreads(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestPoolSize(t *testing.T) {
	// An unconfigured deployment: four backfill writers, four working-set
	// readers, and headroom for the seven light agents and the dashboard.
	if got, want := (Config{}).PoolSize(), 2*DefaultThreads+12; got != want {
		t.Errorf("default PoolSize() = %d, want %d", got, want)
	}
	// A single thread does not shrink the pool below what this app ran on
	// before any of it was configurable.
	if got := (Config{Threads: 1}).PoolSize(); got != 16 {
		t.Errorf("PoolSize() at one thread = %d, want the floor of 16", got)
	}
	// Above the floor, both heavy agents get a connection per thread with
	// headroom left for the seven light ones and the operator's own CRUD.
	for _, threads := range []int{8, 16, 64} {
		got := Config{Threads: threads}.PoolSize()
		if got < 2*threads {
			t.Errorf("PoolSize() with %d threads = %d, too small to run "+
				"%d backfill writers and %d working-set readers at once",
				threads, got, threads, threads)
		}
	}
	// A count past the cap is clamped before it reaches the pool, so a
	// mistyped thread count cannot open an unbounded number of connections.
	if got, want := (Config{Threads: 100_000}).PoolSize(), (Config{Threads: MaxThreads}).PoolSize(); got != want {
		t.Errorf("PoolSize() with an absurd thread count = %d, want %d", got, want)
	}
}
