// Package jobs runs payments-svc's background work as a Kratos server: the
// processor's pass over the rails and payments, and the webhook outbox.
package jobs

import (
	"context"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"

	"omnibus-deposit-network/services/payments-svc/internal/biz"
	"omnibus-deposit-network/services/payments-svc/internal/conf"
	"omnibus-deposit-network/services/payments-svc/internal/metrics"
	"omnibus-deposit-network/services/payments-svc/internal/notify"
)

// Scheduler implements transport.Server so the Kratos app starts and stops it.
type Scheduler struct {
	proc     *biz.Processor
	dispatch *notify.Dispatcher
	tick     time.Duration
	hooks    time.Duration
	log      *log.Helper
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewScheduler creates the scheduler.
func NewScheduler(p *biz.Processor, d *notify.Dispatcher, c *conf.Jobs, logger log.Logger) *Scheduler {
	s := &Scheduler{proc: p, dispatch: d, tick: 2 * time.Second, hooks: time.Second, log: log.NewHelper(logger)}
	if c != nil {
		if c.TickInterval > 0 {
			s.tick = c.TickInterval.Std()
		}
		if c.WebhookInterval > 0 {
			s.hooks = c.WebhookInterval.Std()
		}
	}
	return s
}

// Start runs the loops until Stop.
func (s *Scheduler) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(context.Background())
	s.loop(ctx, "process", s.tick, func(ctx context.Context) error { return s.proc.Tick(ctx) })
	s.loop(ctx, "webhooks", s.hooks, func(ctx context.Context) error {
		n, err := s.dispatch.Flush(ctx)
		metrics.WebhooksDelivered.Add(float64(n))
		return err
	})
	return nil
}

func (s *Scheduler) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context) error) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := fn(ctx); err != nil {
					metrics.JobRuns.WithLabelValues(name, "error").Inc()
					s.log.Errorf("%s: %v", name, err)
				} else {
					metrics.JobRuns.WithLabelValues(name, "ok").Inc()
				}
			}
		}
	}()
}

// Stop ends the loops and waits for the current pass.
func (s *Scheduler) Stop(context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
	return nil
}
