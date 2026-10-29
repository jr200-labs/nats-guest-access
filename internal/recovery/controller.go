// Package recovery owns scheduling and aggregate operational evidence. Durable
// claims and cleanup correctness remain in the access service.
package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jr200-labs/nats-guest-access/internal/access"
)

type Reconciler interface {
	Reconcile(context.Context, int) (access.RecoveryReport, error)
}

type Controller struct {
	service                     Reconciler
	interval                    time.Duration
	batch                       int
	mu                          sync.Mutex
	report                      access.RecoveryReport
	passes, completed, failures uint64
	lastSuccess                 time.Time
}

func New(service Reconciler, interval time.Duration, batch int) *Controller {
	return &Controller{service: service, interval: interval, batch: batch}
}

func (c *Controller) Run(ctx context.Context) {
	if c.interval <= 0 || c.batch < 1 {
		return
	}
	c.Tick(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Tick(ctx)
		}
	}
}

func (c *Controller) Tick(ctx context.Context) {
	bounded, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	report, err := c.service.Reconcile(bounded, c.batch)
	c.mu.Lock()
	c.passes++
	c.completed += uint64(max(0, report.Completed))
	c.failures += uint64(max(0, report.Failed))
	if err == nil {
		c.report = report
		c.lastSuccess = time.Now()
	} else {
		c.failures++
	}
	c.mu.Unlock()
	// No subjects, invitation material, or downstream error strings in logs.
	slog.Info("cleanup recovery", "attempted", report.Attempted, "completed", report.Completed,
		"pending", report.Pending, "failed", report.Failed, "scan_failed", err != nil)
}

func (c *Controller) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Header().Set("Cache-Control", "no-store")
	last := int64(0)
	if !c.lastSuccess.IsZero() {
		last = c.lastSuccess.Unix()
	}
	_, _ = fmt.Fprintf(w, "# TYPE nga_cleanup_passes_total counter\nnga_cleanup_passes_total %d\n"+
		"# TYPE nga_cleanup_completed_total counter\nnga_cleanup_completed_total %d\n"+
		"# TYPE nga_cleanup_failures_total counter\nnga_cleanup_failures_total %d\n"+
		"# TYPE nga_cleanup_pending gauge\nnga_cleanup_pending %d\n"+
		"# TYPE nga_cleanup_oldest_seconds gauge\nnga_cleanup_oldest_seconds %g\n"+
		"# TYPE nga_cleanup_last_success_timestamp_seconds gauge\nnga_cleanup_last_success_timestamp_seconds %d\n",
		c.passes, c.completed, c.failures, c.report.Pending, c.report.OldestSeconds, last)
}
