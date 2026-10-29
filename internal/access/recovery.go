package access

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jr200-labs/nats-guest-access/internal/model"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

var ErrCleanupPending = errors.New("cleanup pending")

// ConfigureCleanupLease must be called before serving requests. Claims outlive
// the bounded application cleanup request; a crashed worker can be retried.
func (s *Service) ConfigureCleanupLease(timeout time.Duration) {
	s.cleanupLease = timeout + 15*time.Second
}

func retryDelay(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	if attempt < 1 {
		attempt = 1
	}
	return time.Duration(1<<uint(attempt-1)) * 5 * time.Second
}

func claimCleanup(progress *model.CleanupProgress, now time.Time, lease time.Duration) (*model.CleanupProgress, error) {
	if progress == nil {
		progress = &model.CleanupProgress{ID: uuid.NewString(), StartedAt: now}
	}
	if now.Before(progress.NextAttemptAt) {
		return nil, ErrCleanupPending
	}
	next := *progress
	next.Attempts++
	next.NextAttemptAt = now.Add(lease)
	return &next, nil
}

func (s *Service) cleanSlot(ctx context.Context, event model.Event, slot model.Slot, revision uint64) error {
	if s.cleaner == nil {
		return errors.New("cleanup is not configured")
	}
	progress, err := claimCleanup(slot.Cleanup, s.now().UTC(), s.cleanupLease)
	if err != nil {
		return err
	}
	slot.Cleanup = progress
	key := slotKey(event.ID, slot.Number)
	revision, err = s.store.Update(ctx, key, encode(slot), revision)
	if err != nil {
		return err
	}
	count, cleanErr := s.cleaner.Cleanup(ctx, progress.ID, event.ApplicationID, event.ID, []string{slot.Subject}, "guest_slot_released")
	if cleanErr == nil && count != 1 {
		cleanErr = errors.New("incomplete cleanup acknowledgement")
	}
	if cleanErr != nil {
		progress.LastError = "application_cleanup_failed"
		progress.NextAttemptAt = s.now().UTC().Add(retryDelay(progress.Attempts))
		_, saveErr := s.store.Update(ctx, key, encode(slot), revision)
		return errors.Join(cleanErr, saveErr)
	}
	if err := deleteIfExists(ctx, s.store, sessionKey(slot.ResumeDigest)); err != nil {
		return err
	}
	// An old response can never delete a new slot generation.
	err = s.store.DeleteRevision(ctx, key, revision)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) receipt(ctx context.Context, eventID string) (model.DeletionReceipt, error) {
	entry, err := s.store.Get(ctx, "receipt."+eventID)
	if errors.Is(err, store.ErrNotFound) {
		return model.DeletionReceipt{}, ErrNotFound
	}
	if err != nil {
		return model.DeletionReceipt{}, err
	}
	var receipt model.DeletionReceipt
	err = json.Unmarshal(entry.Value, &receipt)
	return receipt, err
}

func (s *Service) GetReceipt(ctx context.Context, principal, eventID string) (model.DeletionReceipt, error) {
	receipt, err := s.receipt(ctx, eventID)
	if err != nil {
		return model.DeletionReceipt{}, err
	}
	if principal != receipt.OwnerSubject && principal != receipt.DeletedBySubject {
		return model.DeletionReceipt{}, ErrForbidden
	}
	return receipt, nil
}

func (s *Service) cleanEvent(ctx context.Context, event model.Event, revision uint64) (model.DeletionReceipt, error) {
	receipt, receiptErr := s.receipt(ctx, event.ID)
	if receiptErr != nil && !errors.Is(receiptErr, ErrNotFound) {
		return receipt, receiptErr
	}
	if receiptErr == nil {
		// Preserve the original receipt/count after interruption during finalization.
		return receipt, s.finalizeEvent(ctx, event, revision)
	}
	if s.cleaner == nil {
		return receipt, errors.New("cleanup is not configured")
	}
	if event.Cleanup == nil { // upgrade an already tracked deletion in place
		event.Cleanup = &model.CleanupProgress{ID: event.DeletionID, StartedAt: event.UpdatedAt}
		event.DeletedBySubject = event.OwnerSubject
	}
	progress, err := claimCleanup(event.Cleanup, s.now().UTC(), s.cleanupLease)
	if err != nil {
		return receipt, err
	}
	event.Cleanup = progress
	revision, err = s.store.Update(ctx, eventKey(event.ID), encode(event), revision)
	if err != nil {
		return receipt, err
	}
	entries, err := s.store.List(ctx, "slot."+event.ID+".")
	if err != nil {
		return receipt, err
	}
	subjects := make([]string, 0, len(entries))
	for _, entry := range entries {
		var slot model.Slot
		if err := json.Unmarshal(entry.Value, &slot); err != nil {
			return receipt, err
		}
		subjects = append(subjects, slot.Subject)
	}
	sort.Strings(subjects)
	count, cleanErr := s.cleaner.Cleanup(ctx, event.DeletionID, event.ApplicationID, event.ID, subjects, "demo_event_deleted")
	if cleanErr == nil && count != len(subjects) {
		cleanErr = errors.New("incomplete cleanup acknowledgement")
	}
	if cleanErr != nil {
		progress.LastError = "application_cleanup_failed"
		progress.NextAttemptAt = s.now().UTC().Add(retryDelay(progress.Attempts))
		_, saveErr := s.store.Update(ctx, eventKey(event.ID), encode(event), revision)
		return receipt, errors.Join(cleanErr, saveErr)
	}
	receipt = model.DeletionReceipt{
		DeletionID: event.DeletionID, EventID: event.ID, ApplicationID: event.ApplicationID,
		OwnerSubject: event.OwnerSubject, DeletedBySubject: event.DeletedBySubject,
		CreatedAt: event.CreatedAt, DeletedAt: s.now().UTC(), IdentitiesDeleted: count,
	}
	if _, err = s.store.Create(ctx, "receipt."+event.ID, encode(receipt)); errors.Is(err, store.ErrExists) {
		receipt, err = s.receipt(ctx, event.ID)
	}
	if err != nil {
		return receipt, err
	}
	return receipt, s.finalizeEvent(ctx, event, revision)
}

func (s *Service) finalizeEvent(ctx context.Context, event model.Event, revision uint64) error {
	entries, err := s.store.List(ctx, "slot."+event.ID+".")
	if err != nil {
		return err
	}
	for key, entry := range entries {
		var slot model.Slot
		if err := json.Unmarshal(entry.Value, &slot); err != nil {
			return err
		}
		if err := s.markSessionDeleted(ctx, slot); err != nil {
			return err
		}
		if err := s.store.DeleteRevision(ctx, key, entry.Revision); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if err := deleteIfExists(ctx, s.store, invitationKey(event.InvitationDigest)); err != nil {
		return err
	}
	err = s.store.DeleteRevision(ctx, eventKey(event.ID), revision)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

type RecoveryReport struct {
	Pending       int
	Attempted     int
	Completed     int
	Failed        int
	OldestSeconds float64
}

type cleanupTask struct {
	event    model.Event
	slot     *model.Slot
	revision uint64
	due      time.Time
	started  time.Time
}

// Reconcile never admits users. It retries durable intents and expired slots,
// with an oldest-due-first batch so a failed application cannot starve the rest.
func (s *Service) Reconcile(ctx context.Context, batch int) (RecoveryReport, error) {
	report := RecoveryReport{}
	if batch < 1 || s.cleaner == nil {
		return report, nil
	}
	events, err := s.store.List(ctx, "event.")
	if err != nil {
		return report, err
	}
	tasks := []cleanupTask{}
	now := s.now().UTC()
	for _, entry := range events {
		var event model.Event
		if err := json.Unmarshal(entry.Value, &event); err != nil {
			return report, fmt.Errorf("decode recovery event: %w", err)
		}
		if event.State == model.EventDeleting {
			task := cleanupTask{event: event, revision: entry.Revision, started: event.UpdatedAt}
			if event.Cleanup != nil {
				task.due = event.Cleanup.NextAttemptAt
				task.started = event.Cleanup.StartedAt
			}
			tasks = append(tasks, task)
			continue
		}
		slots, err := s.store.List(ctx, "slot."+event.ID+".")
		if err != nil {
			return report, err
		}
		for _, entry := range slots {
			var slot model.Slot
			if err := json.Unmarshal(entry.Value, &slot); err != nil {
				return report, err
			}
			task := cleanupTask{event: event, slot: &slot, revision: entry.Revision, due: slot.GraceExpiresAt, started: slot.GraceExpiresAt}
			if slot.Cleanup != nil {
				task.due, task.started = slot.Cleanup.NextAttemptAt, slot.Cleanup.StartedAt
			} else if event.State != model.EventOpen || slot.Reserved || now.Before(slot.GraceExpiresAt) {
				continue
			}
			tasks = append(tasks, task)
		}
	}
	report.Pending = len(tasks)
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].due.Before(tasks[j].due) })
	for _, task := range tasks {
		report.OldestSeconds = max(report.OldestSeconds, now.Sub(task.started).Seconds())
		if now.Before(task.due) || report.Attempted >= batch {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Attempted++
		if task.slot == nil {
			_, err = s.cleanEvent(ctx, task.event, task.revision)
		} else {
			err = s.cleanSlot(ctx, task.event, *task.slot, task.revision)
		}
		if err == nil {
			report.Completed++
			report.Pending--
		} else if !errors.Is(err, store.ErrConflict) && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, ErrCleanupPending) {
			report.Failed++
		}
	}
	return report, nil
}

func (s *Service) ListReceipts(ctx context.Context, principal string) ([]model.DeletionReceipt, error) {
	entries, err := s.store.List(ctx, "receipt.")
	if err != nil {
		return nil, err
	}
	result := []model.DeletionReceipt{}
	for _, entry := range entries {
		var receipt model.DeletionReceipt
		if err := json.Unmarshal(entry.Value, &receipt); err != nil {
			return nil, err
		}
		if principal == receipt.OwnerSubject || principal == receipt.DeletedBySubject {
			result = append(result, receipt)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].DeletedAt.After(result[j].DeletedAt) })
	if len(result) > 100 {
		result = result[:100]
	}
	return result, nil
}
