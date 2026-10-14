// Package engine isolates Docker operations from lifecycle coordination.
package engine

import (
	"context"
	"site-deploy/internal/model"
)

type Status struct {
	Running   bool
	Suspended bool
	// Partial identifies live project resources that are neither ready to
	// serve nor a complete, deliberately paused stack.
	Partial  bool
	Upstream string
}

type Runtime interface {
	Up(context.Context, model.App) (string, error)
	Suspend(context.Context, model.App) error
	Resume(context.Context, model.App) (string, error)
	Down(context.Context, model.App) error
	Inspect(context.Context, model.App) (Status, error)
	Logs(context.Context, model.App, int) (string, error)
}
