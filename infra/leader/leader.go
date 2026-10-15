// Package leader runs leader-only tasks on the elected email-service instance.
package leader

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/webitel/webitel-go-kit/infra/discovery/consul"
	"go.uber.org/fx"
	"golang.org/x/sync/errgroup"
)

// TaskGroup is the fx value group of leader-only tasks.
const TaskGroup = `group:"leader_tasks"`

// Task is leader-only work; Run must return once ctx is canceled.
type Task interface {
	Name() string
	Run(ctx context.Context) error
}

// Params are the dependencies of Runner.
type Params struct {
	fx.In

	Elector   consul.LeadershipElector
	Tasks     []Task `group:"leader_tasks"`
	Log       *slog.Logger
	Lifecycle fx.Lifecycle
}

// Runner holds one leadership term for all tasks, so they always run on the same instance.
type Runner struct {
	elector consul.LeadershipElector
	tasks   []Task
	log     *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// New creates a Runner and ties it to the application lifecycle.
func New(p Params) *Runner {
	r := &Runner{
		elector: p.Elector,
		tasks:   p.Tasks,
		log:     p.Log.With("component", "leader"),
		done:    make(chan struct{}),
	}

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			r.start()

			return nil
		},
		OnStop: r.stop,
	})

	return r
}

func (r *Runner) start() {
	if len(r.tasks) == 0 {
		close(r.done)

		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel

	go func() {
		defer close(r.done)

		r.elector.Run(ctx, r.lead, func() {
			r.log.Warn("leader tasks stopped")
		})
	}()
}

// stop ends the election; the elector stops the tasks before releasing the key.
func (r *Runner) stop(ctx context.Context) error {
	if r.cancel != nil {
		r.cancel()
	}

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("leader: stop: %w", ctx.Err())
	}
}

// lead runs every task for one leadership term; a failed task ends the term.
func (r *Runner) lead(ctx context.Context) error {
	r.log.Info("leader tasks started", "tasks", len(r.tasks))

	group, ctx := errgroup.WithContext(ctx)
	for _, task := range r.tasks {
		group.Go(func() error {
			if err := task.Run(ctx); err != nil {
				return fmt.Errorf("leader task %s: %w", task.Name(), err)
			}

			return nil
		})
	}

	return group.Wait()
}
