package access

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jr200-labs/nats-guest-access/internal/issuer"
	"github.com/jr200-labs/nats-guest-access/internal/model"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

type fakeCleaner struct {
	mu       sync.Mutex
	subjects []string
}

func (f *fakeCleaner) Cleanup(_ context.Context, _, _, _ string, subjects []string, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subjects = append(f.subjects, subjects...)
	return len(subjects), nil
}

func newTestService(t *testing.T, capacity int) (*Service, *fakeCleaner) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokenIssuer, err := issuer.New(key, "https://guest.example", "whengas-demo", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cleaner := &fakeCleaner{}
	return New(store.NewMemory(), tokenIssuer, strings.Repeat("h", 32), "https://www.whengas.com/demo", capacity, 30*time.Second, 5*time.Minute, cleaner), cleaner
}

func createOpenEvent(t *testing.T, service *Service, capacity int) EventWithInvitation {
	t.Helper()
	created, err := service.CreateEvent(context.Background(), "presenter", CreateEventInput{
		ApplicationID: "whengas", Name: "Talk", Capacity: capacity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetState(context.Background(), "presenter", created.Event.ID, model.EventOpen); err != nil {
		t.Fatal(err)
	}
	return created
}

func invitationSecret(url string) string {
	_, secret, _ := strings.Cut(url, "#")
	return secret
}

func TestCapacityIsAtomic(t *testing.T) {
	service, _ := newTestService(t, 10)
	event := createOpenEvent(t, service, 10)
	secret := invitationSecret(event.InvitationURL)

	var wg sync.WaitGroup
	results := make(chan error, 50)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.StartSession(context.Background(), secret)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrCapacity) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if successes != 10 {
		t.Fatalf("got %d admitted sessions, want 10", successes)
	}
}

func TestPausePreservesSessionUntilReopened(t *testing.T) {
	service, _ := newTestService(t, 1)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	event := createOpenEvent(t, service, 1)
	session, err := service.StartSession(context.Background(), invitationSecret(event.InvitationURL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetState(context.Background(), "presenter", event.Event.ID, model.EventPaused); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := service.RenewSession(context.Background(), session.ResumeSecret); !errors.Is(err, ErrAdmissionClosed) {
		t.Fatalf("renew while paused: got %v", err)
	}
	if _, err := service.SetState(context.Background(), "presenter", event.Event.ID, model.EventOpen); err != nil {
		t.Fatal(err)
	}
	resumed, err := service.RenewSession(context.Background(), session.ResumeSecret)
	if err != nil {
		t.Fatalf("resume after reopen: %v", err)
	}
	if resumed.Subject != session.Subject || resumed.Slot != session.Slot {
		t.Fatalf("resumed a different guest: %+v", resumed)
	}
}

func TestExpiredSlotIsPurgedBeforeReuse(t *testing.T) {
	service, cleaner := newTestService(t, 1)
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	event := createOpenEvent(t, service, 1)
	first, err := service.StartSession(context.Background(), invitationSecret(event.InvitationURL))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Minute)
	second, err := service.StartSession(context.Background(), invitationSecret(event.InvitationURL))
	if err != nil {
		t.Fatal(err)
	}
	if first.Subject == second.Subject {
		t.Fatal("reused a guest subject")
	}
	cleaner.mu.Lock()
	defer cleaner.mu.Unlock()
	if len(cleaner.subjects) != 1 || cleaner.subjects[0] != first.Subject {
		t.Fatalf("unexpected cleanup subjects: %v", cleaner.subjects)
	}
}

func TestDeleteRequiresPermissionAndLeavesReceipt(t *testing.T) {
	service, _ := newTestService(t, 1)
	event := createOpenEvent(t, service, 1)
	if _, err := service.StartSession(context.Background(), invitationSecret(event.InvitationURL)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DeleteEvent(context.Background(), "stranger", event.Event.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("delete by stranger: got %v", err)
	}
	receipt, err := service.DeleteEvent(context.Background(), "presenter", event.Event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.IdentitiesDeleted != 1 {
		t.Fatalf("deleted %d identities, want 1", receipt.IdentitiesDeleted)
	}
	if _, err := service.GetEvent(context.Background(), "presenter", event.Event.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted event still available: %v", err)
	}
}
