package recovery

import (
	"context"
	"errors"
	"github.com/jr200-labs/nats-guest-access/internal/access"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fake struct{ err error }

func (f *fake) Reconcile(context.Context, int) (access.RecoveryReport, error) {
	return access.RecoveryReport{Pending: 2, Attempted: 1, Completed: 1, OldestSeconds: 120}, f.err
}
func TestMetricsRetainBacklogOnScanFailure(t *testing.T) {
	source := &fake{}
	controller := New(source, time.Second, 25)
	controller.Tick(context.Background())
	source.err = errors.New("private details")
	controller.Tick(context.Background())
	recorder := httptest.NewRecorder()
	controller.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{"nga_cleanup_pending 2", "nga_cleanup_failures_total 1", "nga_cleanup_oldest_seconds 120"} {
		if !strings.Contains(body, want) {
			t.Fatal(body)
		}
	}
	if strings.Contains(body, "private") {
		t.Fatal("error content in metrics")
	}
}
