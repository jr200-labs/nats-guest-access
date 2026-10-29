package access

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jr200-labs/nats-guest-access/internal/model"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

type cleanupFunc func(context.Context, string, string, string, []string, string) (int, error)

func (f cleanupFunc) Cleanup(ctx context.Context, id, app, event string, subjects []string, reason string) (int, error) {
	return f(ctx, id, app, event, subjects, reason)
}

func TestRecoveryRetriesStableDeletionAfterRestartWithoutReopening(t *testing.T) {
	s, _ := newTestService(t, 2)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	event := createOpenEvent(t, s, 2)
	guest, err := s.StartSession(ctx, invitationSecret(event.InvitationURL))
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	var firstID string
	s.cleaner = cleanupFunc(func(_ context.Context, id, _, _ string, subjects []string, _ string) (int, error) {
		calls++
		if calls == 1 {
			firstID = id
			return 0, errors.New("sensitive downstream content")
		}
		if id != firstID {
			t.Fatal("retry changed deletion identity")
		}
		return len(subjects), nil
	})
	if _, err := s.DeleteEvent(ctx, "presenter", event.Event.ID); err == nil {
		t.Fatal("expected failure")
	}
	tracked, _, err := s.loadEvent(ctx, event.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tracked.State != model.EventDeleting || tracked.Cleanup.Attempts != 1 || strings.Contains(string(encode(tracked)), "sensitive") {
		t.Fatalf("bad progress: %+v", tracked)
	}
	if _, err := s.RenewSession(ctx, guest.ResumeSecret); !errors.Is(err, ErrEventDeleted) {
		t.Fatalf("renew: %v", err)
	}
	if _, err := s.SetState(ctx, "presenter", event.Event.ID, model.EventOpen); !errors.Is(err, ErrConflict) {
		t.Fatalf("reopen: %v", err)
	}
	report, err := s.Reconcile(ctx, 25)
	if err != nil || report.Attempted != 0 {
		t.Fatalf("backoff: %+v %v", report, err)
	}
	restarted := New(s.store, s.issuer, string(s.hmacKey), s.publicDemoURL, 0, s.leaseTTL, s.gracePeriod, s.cleaner)
	now = now.Add(time.Minute)
	restarted.now = s.now
	report, err = restarted.Reconcile(ctx, 25)
	if err != nil || report.Completed != 1 {
		t.Fatalf("recovery: %+v %v", report, err)
	}
	receipt, err := restarted.GetReceipt(ctx, "presenter", event.Event.ID)
	if err != nil || receipt.DeletionID != firstID || receipt.IdentitiesDeleted != 1 {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	if _, err := restarted.GetReceipt(ctx, "stranger", event.Event.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("receipt leaked: %v", err)
	}
}

func TestSlotClaimPreventsRenewalAndReuseUntilCompleteAcknowledgement(t *testing.T) {
	s, _ := newTestService(t, 1)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	event := createOpenEvent(t, s, 1)
	first, err := s.StartSession(ctx, invitationSecret(event.InvitationURL))
	if err != nil {
		t.Fatal(err)
	}
	var cleanupID string
	s.cleaner = cleanupFunc(func(_ context.Context, id, _, _ string, _ []string, _ string) (int, error) {
		cleanupID = id
		if _, err := s.RenewSession(ctx, first.ResumeSecret); !errors.Is(err, ErrSessionExpired) {
			t.Fatalf("renew while erasing: %v", err)
		}
		return 0, nil // partial acknowledgement must not release the slot
	})
	if err := s.ReleaseSlot(ctx, "presenter", event.Event.ID, 1); err == nil {
		t.Fatal("partial acknowledgement accepted")
	}
	if _, err := s.StartSession(ctx, invitationSecret(event.InvitationURL)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reused before cleanup: %v", err)
	}
	now = now.Add(time.Minute)
	s.cleaner = cleanupFunc(func(_ context.Context, id, _, _ string, subjects []string, _ string) (int, error) {
		if id != cleanupID {
			t.Fatal("slot deletion ID changed")
		}
		return len(subjects), nil
	})
	report, err := s.Reconcile(ctx, 1)
	if err != nil || report.Completed != 1 {
		t.Fatalf("recovery: %+v %v", report, err)
	}
	next, err := s.StartSession(ctx, invitationSecret(event.InvitationURL))
	if err != nil || next.Subject == first.Subject {
		t.Fatalf("replacement: %+v %v", next, err)
	}
}

type interruptedStore struct {
	store.Store
	fail bool
}

func (s *interruptedStore) DeleteRevision(ctx context.Context, key string, rev uint64) error {
	if s.fail && strings.HasPrefix(key, "event.") {
		s.fail = false
		return errors.New("interrupted after deleting slots")
	}
	return s.Store.DeleteRevision(ctx, key, rev)
}

func TestReceiptSurvivesInterruptedFinalization(t *testing.T) {
	s, cleaner := newTestService(t, 1)
	ctx := context.Background()
	s.store = &interruptedStore{Store: s.store, fail: true}
	event := createOpenEvent(t, s, 1)
	if _, err := s.StartSession(ctx, invitationSecret(event.InvitationURL)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteEvent(ctx, "presenter", event.Event.ID); err == nil {
		t.Fatal("expected interrupted finalization")
	}
	receipt, err := s.DeleteEvent(ctx, "presenter", event.Event.ID)
	if err != nil || receipt.IdentitiesDeleted != 1 {
		t.Fatalf("lost original receipt: %+v %v", receipt, err)
	}
	if len(cleaner.subjects) != 1 {
		t.Fatal("cleanup unnecessarily replayed after receipt")
	}
}

func TestRecoveryBoundsBatchAndPreservesPausedSlots(t *testing.T) {
	s, _ := newTestService(t, 1)
	ctx := context.Background()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		event := createOpenEvent(t, s, 1)
		if _, err := s.StartSession(ctx, invitationSecret(event.InvitationURL)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := s.SetState(ctx, "presenter", event.Event.ID, model.EventPaused); err != nil {
				t.Fatal(err)
			}
		}
	}
	now = now.Add(time.Hour)
	report, err := s.Reconcile(ctx, 1)
	if err != nil || report.Attempted != 1 || report.Completed != 1 || report.Pending != 1 {
		t.Fatalf("bounded scan: %+v %v", report, err)
	}
	report, err = s.Reconcile(ctx, 10)
	if err != nil || report.Completed != 1 || report.Pending != 0 {
		t.Fatalf("remaining scan: %+v %v", report, err)
	}
	entries, err := s.store.List(ctx, "slot.")
	if err != nil || len(entries) != 1 {
		t.Fatalf("paused slot removed: %v %v", entries, err)
	}
}

func TestReceiptListingIsScopedToDeletingPrincipal(t *testing.T) {
	s, _ := newTestService(t, 1)
	ctx := context.Background()
	event := createOpenEvent(t, s, 1)
	if _, err := s.DeleteEvent(ctx, "presenter", event.Event.ID); err != nil {
		t.Fatal(err)
	}
	own, err := s.ListReceipts(ctx, "presenter")
	if err != nil || len(own) != 1 {
		t.Fatalf("own receipts: %v %v", own, err)
	}
	other, err := s.ListReceipts(ctx, "other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other receipts: %v %v", other, err)
	}
}
