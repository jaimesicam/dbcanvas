package main

import (
	"context"
	"sync"
	"time"
)

// A stack's provisioning happens in background goroutines that outlive the
// deploy request. Without a handle on them, destroying a stack mid-deploy left
// those goroutines running against containers that no longer existed: they
// retried `docker exec` until it gave up, then wrote DeployError + a failure
// notification — landing on the *next* deploy's freshly created rows and
// wedging nodes in "error". deployRun gives every provisioner a cancellable,
// per-stack context and lets teardown wait for them to exit first.
type deployRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// cancelDeployWait bounds how long teardown waits for provisioning goroutines to
// notice cancellation and return before it removes containers anyway.
const cancelDeployWait = 45 * time.Second

// beginDeploy registers a run for the stack. ok is false when a deploy is
// already in flight for it (the caller should reject the request rather than
// race a second set of provisioners onto the same nodes).
func (a *App) beginDeploy(stackID int64) (*deployRun, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &deployRun{ctx: ctx, cancel: cancel}
	if _, loaded := a.deploys.LoadOrStore(stackID, r); loaded {
		cancel()
		return nil, false
	}
	return r, true
}

// abortDeploy drops a run registered by beginDeploy that never spawned any
// provisioners (an early error return from the deploy handler).
func (a *App) abortDeploy(stackID int64, r *deployRun) {
	r.cancel()
	a.deploys.CompareAndDelete(stackID, r)
}

// finishDeploy releases the run once every provisioning goroutine has returned,
// so a later deploy can start. Call it after all provisioners were spawned —
// each has already registered with the WaitGroup via deployScope by then.
func (a *App) finishDeploy(stackID int64, r *deployRun) {
	go func() {
		r.wg.Wait()
		r.cancel()
		a.deploys.CompareAndDelete(stackID, r)
	}()
}

// deployScope is called by a provisioner *synchronously*, before it spawns its
// goroutine: it returns the stack's cancellable context and the done func that
// goroutine must defer. Calling it before the `go` statement is what makes the
// WaitGroup counter reliable — every provisioner has registered by the time the
// deploy handler returns, so finishDeploy's Wait cannot race an Add.
//
// Falls back to a background context when no run is registered (a provisioner
// invoked outside a deploy, e.g. a management action).
func (a *App) deployScope(stackID int64) (context.Context, func()) {
	v, ok := a.deploys.Load(stackID)
	if !ok {
		return context.Background(), func() {}
	}
	r := v.(*deployRun)
	// Already cancelled (a destroy landed while the handler was still spawning
	// provisioners): hand back the cancelled context without joining the
	// WaitGroup, so we never Add to a group that cancelDeploy is Waiting on.
	if r.ctx.Err() != nil {
		return r.ctx, func() {}
	}
	r.wg.Add(1)
	return r.ctx, r.wg.Done
}

// deployCancelled reports whether the stack's in-flight deploy was cancelled
// (i.e. the stack is being destroyed). Provisioners use it to stay quiet rather
// than reporting failures that the teardown itself caused.
func (a *App) deployCancelled(stackID int64) bool {
	v, ok := a.deploys.Load(stackID)
	if !ok {
		return false
	}
	return v.(*deployRun).ctx.Err() != nil
}

// cancelDeploy cancels the stack's in-flight provisioning and waits (bounded by
// cancelDeployWait) for its goroutines to exit, so the caller can safely remove
// the stack's containers without racing them. No-op when nothing is in flight.
func (a *App) cancelDeploy(stackID int64) {
	v, ok := a.deploys.Load(stackID)
	if !ok {
		return
	}
	r := v.(*deployRun)
	r.cancel()
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cancelDeployWait):
	}
	a.deploys.CompareAndDelete(stackID, r)
}
