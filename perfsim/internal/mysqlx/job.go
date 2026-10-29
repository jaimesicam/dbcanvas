package mysqlx

import (
	"context"
	"errors"
	"sync"

	"perfsim/internal/core"
)

// job is the Running every dial returns. The dial's work runs in one goroutine
// under ctx; Stop cancels it, waits for it to return, and then runs the release
// steps (UNLOCK TABLES, START REPLICA, restoring a setting) in the order they
// were registered, newest first. Release always runs, also when the work failed
// half-way, so nothing is left held.
type job struct {
	mu       sync.Mutex
	st       core.RunStatus
	holders  map[string][]string
	releases []func(context.Context) error
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	stopErr  error
	params   core.Params
}

func newJob(parent context.Context, p core.Params, work func(ctx context.Context, j *job)) *job {
	ctx, cancel := context.WithCancel(parent)
	j := &job{st: core.RunStatus{Progress: -1}, holders: map[string][]string{}, cancel: cancel, done: make(chan struct{}), params: p}
	go func() {
		defer close(j.done)
		work(ctx, j)
	}()
	return j
}

func (j *job) set(phase string, progress float64, note string) {
	j.mu.Lock()
	j.st.Phase, j.st.Progress, j.st.Note = phase, progress, note
	j.mu.Unlock()
}

func (j *job) note(note string) {
	j.mu.Lock()
	j.st.Note = note
	j.mu.Unlock()
}

// finish marks the dial as having ended on its own; the hub then stops it,
// which runs the release steps.
func (j *job) finish(err error) {
	j.mu.Lock()
	j.st.Done = true
	if err != nil && !errors.Is(err, context.Canceled) {
		j.st.Err = err.Error()
	}
	j.mu.Unlock()
}

// fail records an error without ending a dial that has other members to hold.
func (j *job) fail(err error) {
	j.mu.Lock()
	if j.st.Err == "" {
		j.st.Err = err.Error()
	}
	j.mu.Unlock()
}

func (j *job) hold(member, connID string) {
	j.mu.Lock()
	j.holders[member] = append(j.holders[member], connID)
	j.mu.Unlock()
}

func (j *job) onRelease(f func(context.Context) error) {
	j.mu.Lock()
	j.releases = append(j.releases, f)
	j.mu.Unlock()
}

func (j *job) param() core.Params {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.params
}

// Update changes a running dial's settings; loops read them on each turn.
func (j *job) Update(p core.Params) {
	j.mu.Lock()
	j.params = p
	j.mu.Unlock()
}

func (j *job) Status() core.RunStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st
}

func (j *job) Holders() map[string][]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string][]string{}
	for k, v := range j.holders {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (j *job) Stop(ctx context.Context) error {
	j.once.Do(func() {
		j.cancel()
		select {
		case <-j.done:
		case <-ctx.Done():
		}
		j.mu.Lock()
		rel := j.releases
		j.releases = nil
		j.mu.Unlock()
		var errs []error
		for i := len(rel) - 1; i >= 0; i-- {
			if err := rel[i](ctx); err != nil {
				errs = append(errs, err)
			}
		}
		j.stopErr = errors.Join(errs...)
	})
	return j.stopErr
}
