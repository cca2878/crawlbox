package app

import (
	"context"
	"errors"
	"log/slog"
)

var ErrRunNotActive = errors.New("run is no longer active")
var ErrRunSealed = errors.New("run is snapshotting, committing or finishing; cancellation is no longer available")
var ErrRunStopped = errors.New("run stopped by administrator")

type runControl struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	sealed bool // protected by App.mu; cancellation cannot cross publication
}

func (a *App) CanCancel(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.controls[id]
	return c != nil && !c.sealed && c.ctx.Err() == nil
}

// Cancel targets a run ID, never whichever run currently belongs to a source.
// Success means cancellation was requested; cleanup finishes before the slot
// and source become available to another run.
func (a *App) Cancel(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.controls[id]
	if c == nil {
		return ErrRunNotActive
	}
	if c.sealed {
		return ErrRunSealed
	}
	c.cancel(ErrRunStopped)
	slog.Info("collection cancellation requested", "run", id)
	return nil
}

func (a *App) beginPublication(c *runControl) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := c.ctx.Err(); err != nil {
		return err
	}
	c.sealed = true
	return nil
}

func (a *App) CancellationRequested(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.controls[id]
	return c != nil && errors.Is(context.Cause(c.ctx), ErrRunStopped)
}
