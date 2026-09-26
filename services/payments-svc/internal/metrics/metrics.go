// Package metrics exposes Prometheus metrics for payments-svc.
package metrics

import (
	"context"
	"strconv"
	"time"

	"github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/middleware"
	"github.com/go-kratos/kratos/v2/transport"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "payments", Name: "requests_total", Help: "API requests by operation and result code.",
	}, []string{"operation", "code"})
	latency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "payments", Name: "request_duration_seconds", Help: "API request latency.", Buckets: prometheus.DefBuckets,
	}, []string{"operation"})

	// JobRuns counts background passes by job and outcome.
	JobRuns = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "payments", Name: "job_runs_total", Help: "Background job passes by job and outcome.",
	}, []string{"job", "outcome"})

	// WebhooksDelivered counts successful webhook deliveries.
	WebhooksDelivered = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "payments", Name: "webhooks_delivered_total", Help: "Webhook deliveries acknowledged with 2xx.",
	})
)

// Server records each operation's count and latency.
func Server() middleware.Middleware {
	return func(next middleware.Handler) middleware.Handler {
		return func(ctx context.Context, req any) (any, error) {
			op := "unknown"
			if tr, ok := transport.FromServerContext(ctx); ok {
				op = tr.Operation()
			}
			start := time.Now()
			reply, err := next(ctx, req)
			code := 200
			if err != nil {
				code = int(errors.FromError(err).Code)
			}
			requests.WithLabelValues(op, strconv.Itoa(code)).Inc()
			latency.WithLabelValues(op).Observe(time.Since(start).Seconds())
			return reply, err
		}
	}
}
