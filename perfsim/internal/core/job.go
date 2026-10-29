package core

import (
	"context"
	"errors"
	"sync"
)

// Job is the Running a dial returns. The dial's work runs in one goroutine
// under ctx; Stop cancels it, waits for it to return, and then runs the release
// steps (UNLOCK TABLES, START REPLICA, restoring a setting) in the order they
// were registered, newest first. Release always runs, also when the work failed
// half-way, so nothing is left held.
type Job struct {
	mu       sync.Mutex
	st       RunStatus
	holders  map[string][]string
	releases []func(context.Context) error
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
	stopErr  error
	params   Params
}

// NewJob starts work in its own goroutine.
func NewJob(parent context.Context, p Params, work func(ctx context.Context, j *Job)) *Job {
	ctx, cancel := context.WithCancel(parent)
	j := &Job{st: RunStatus{Progress: -1}, holders: map[string][]string{}, cancel: cancel, done: make(chan struct{}), params: p}
	go func() {
		defer close(j.done)
		work(ctx, j)
	}()
	return j
}

func (j *Job) Set(phase string, progress float64, note string) {
	j.mu.Lock()
	j.st.Phase, j.st.Progress, j.st.Note = phase, progress, note
	j.mu.Unlock()
}

func (j *Job) Note(note string) {
	j.mu.Lock()
	j.st.Note = note
	j.mu.Unlock()
}

// finish marks the dial as having ended on its own; the hub then stops it,
// which runs the release steps.
func (j *Job) Finish(err error) {
	j.mu.Lock()
	j.st.Done = true
	if err != nil && !errors.Is(err, context.Canceled) {
		j.st.Err = err.Error()
	}
	j.mu.Unlock()
}

// fail records an error without ending a dial that has other members to hold.
func (j *Job) Fail(err error) {
	j.mu.Lock()
	if j.st.Err == "" {
		j.st.Err = err.Error()
	}
	j.mu.Unlock()
}

func (j *Job) Hold(member, connID string) {
	j.mu.Lock()
	j.holders[member] = append(j.holders[member], connID)
	j.mu.Unlock()
}

func (j *Job) OnRelease(f func(context.Context) error) {
	j.mu.Lock()
	j.releases = append(j.releases, f)
	j.mu.Unlock()
}

func (j *Job) Param() Params {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.params
}

// Update changes a running dial's settings; loops read them on each turn.
func (j *Job) Update(p Params) {
	j.mu.Lock()
	j.params = p
	j.mu.Unlock()
}

func (j *Job) Status() RunStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.st
}

func (j *Job) Holders() map[string][]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string][]string{}
	for k, v := range j.holders {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (j *Job) Stop(ctx context.Context) error {
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
