package access

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jr200-labs/nats-guest-access/internal/issuer"
	"github.com/jr200-labs/nats-guest-access/internal/model"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

var (
	ErrNotFound        = errors.New("not found")
	ErrForbidden       = errors.New("forbidden")
	ErrConflict        = errors.New("conflict")
	ErrCapacity        = errors.New("event is at capacity")
	ErrAdmissionClosed = errors.New("admission is closed")
	ErrSessionExpired  = errors.New("session expired")
)

var applicationIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

type Cleaner interface {
	Cleanup(context.Context, string, string, string, []string, string) (int, error)
}

type Service struct {
	store         store.Store
	issuer        *issuer.Issuer
	hmacKey       []byte
	publicDemoURL string
	maxCapacity   int
	leaseTTL      time.Duration
	gracePeriod   time.Duration
	cleaner       Cleaner
	now           func() time.Time
}

type CreateEventInput struct {
	ApplicationID         string `json:"application_id"`
	Name                  string `json:"name"`
	Capacity              int    `json:"capacity"`
	InspectLiveWorkspaces bool   `json:"inspect_live_workspaces"`
}

type EventWithInvitation struct {
	Event         model.Event `json:"event"`
	InvitationURL string      `json:"invitation_url"`
}

type Session struct {
	EventID        string    `json:"event_id"`
	Subject        string    `json:"subject"`
	DisplayName    string    `json:"display_name"`
	Slot           int       `json:"slot"`
	Token          string    `json:"token"`
	TokenExpiresAt time.Time `json:"token_expires_at"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	ResumeSecret   string    `json:"-"`
}

func New(st store.Store, tokenIssuer *issuer.Issuer, hmacKey, publicDemoURL string, maxCapacity int, leaseTTL, gracePeriod time.Duration, cleaner Cleaner) *Service {
	return &Service{
		store: st, issuer: tokenIssuer, hmacKey: []byte(hmacKey),
		publicDemoURL: strings.TrimRight(publicDemoURL, "/"), maxCapacity: maxCapacity,
		leaseTTL: leaseTTL, gracePeriod: gracePeriod, cleaner: cleaner, now: time.Now,
	}
}

func (s *Service) CreateEvent(ctx context.Context, owner string, input CreateEventInput) (EventWithInvitation, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.ApplicationID = strings.TrimSpace(input.ApplicationID)
	if owner == "" || input.Name == "" || len(input.Name) > 200 || !applicationIDPattern.MatchString(input.ApplicationID) || input.Capacity < 0 || input.Capacity > s.maxCapacity {
		return EventWithInvitation{}, fmt.Errorf("%w: invalid event", ErrConflict)
	}
	secret, err := randomSecret()
	if err != nil {
		return EventWithInvitation{}, err
	}
	now := s.now().UTC()
	event := model.Event{
		ID: uuid.NewString(), ApplicationID: input.ApplicationID, Name: input.Name,
		OwnerSubject: owner, Capacity: input.Capacity,
		InspectLiveWorkspaces: input.InspectLiveWorkspaces, State: model.EventDraft,
		InvitationDigest: s.digest(secret), Cohosts: map[string][]model.Capability{},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := s.store.Create(ctx, eventKey(event.ID), encode(event)); err != nil {
		return EventWithInvitation{}, err
	}
	if _, err := s.store.Create(ctx, invitationKey(event.InvitationDigest), []byte(event.ID)); err != nil {
		_ = s.store.Delete(ctx, eventKey(event.ID))
		return EventWithInvitation{}, err
	}
	return EventWithInvitation{Event: event, InvitationURL: s.invitationURL(secret)}, nil
}

func (s *Service) ListEvents(ctx context.Context, principal string) ([]model.Event, error) {
	entries, err := s.store.List(ctx, "event.")
	if err != nil {
		return nil, err
	}
	events := make([]model.Event, 0)
	for _, entry := range entries {
		var event model.Event
		if json.Unmarshal(entry.Value, &event) == nil && (event.OwnerSubject == principal || event.Cohosts[principal] != nil) {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].CreatedAt.Before(events[j].CreatedAt) })
	return events, nil
}

func (s *Service) GetEvent(ctx context.Context, principal, eventID string) (model.Event, error) {
	event, _, err := s.loadEvent(ctx, eventID)
	if err != nil {
		return model.Event{}, err
	}
	if event.OwnerSubject != principal && event.Cohosts[principal] == nil {
		return model.Event{}, ErrForbidden
	}
	return event, nil
}

func (s *Service) SetState(ctx context.Context, principal, eventID string, target model.EventState) (model.Event, error) {
	for attempt := 0; attempt < 5; attempt++ {
		event, revision, err := s.loadEvent(ctx, eventID)
		if err != nil {
			return model.Event{}, err
		}
		if !can(event, principal, model.CapabilityOperate) {
			return model.Event{}, ErrForbidden
		}
		if event.State != target && !validTransition(event.State, target) {
			return model.Event{}, fmt.Errorf("%w: cannot transition %s to %s", ErrConflict, event.State, target)
		}
		event.State = target
		event.UpdatedAt = s.now().UTC()
		if _, err = s.store.Update(ctx, eventKey(eventID), encode(event), revision); errors.Is(err, store.ErrConflict) {
			continue
		} else if err != nil {
			return model.Event{}, err
		}
		if target == model.EventPaused {
			if err := s.reserveSlots(ctx, eventID); err != nil {
				return model.Event{}, err
			}
		}
		return event, nil
	}
	return model.Event{}, ErrConflict
}

func (s *Service) UpdateCapacity(ctx context.Context, principal, eventID string, capacity int) (model.Event, error) {
	if capacity < 0 || capacity > s.maxCapacity {
		return model.Event{}, fmt.Errorf("%w: invalid capacity", ErrConflict)
	}
	for attempt := 0; attempt < 5; attempt++ {
		event, revision, err := s.loadEvent(ctx, eventID)
		if err != nil {
			return model.Event{}, err
		}
		if !can(event, principal, model.CapabilityOperate) {
			return model.Event{}, ErrForbidden
		}
		event.Capacity = capacity
		event.UpdatedAt = s.now().UTC()
		if _, err = s.store.Update(ctx, eventKey(eventID), encode(event), revision); errors.Is(err, store.ErrConflict) {
			continue
		} else if err != nil {
			return model.Event{}, err
		}
		return event, nil
	}
	return model.Event{}, ErrConflict
}

func (s *Service) RotateInvitation(ctx context.Context, principal, eventID string) (string, error) {
	event, revision, err := s.loadEvent(ctx, eventID)
	if err != nil {
		return "", err
	}
	if !can(event, principal, model.CapabilityOperate) {
		return "", ErrForbidden
	}
	secret, err := randomSecret()
	if err != nil {
		return "", err
	}
	newDigest := s.digest(secret)
	if _, err = s.store.Create(ctx, invitationKey(newDigest), []byte(eventID)); err != nil {
		return "", err
	}
	oldDigest := event.InvitationDigest
	event.InvitationDigest = newDigest
	event.UpdatedAt = s.now().UTC()
	if _, err = s.store.Update(ctx, eventKey(eventID), encode(event), revision); err != nil {
		_ = s.store.Delete(ctx, invitationKey(newDigest))
		return "", err
	}
	_ = s.store.Delete(ctx, invitationKey(oldDigest))
	return s.invitationURL(secret), nil
}

func (s *Service) SetCohost(ctx context.Context, principal, eventID, cohost string, capabilities []model.Capability) (model.Event, error) {
	for _, capability := range capabilities {
		if capability != model.CapabilityOperate && capability != model.CapabilityInspect && capability != model.CapabilityDelete {
			return model.Event{}, fmt.Errorf("%w: invalid capability", ErrConflict)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		event, revision, err := s.loadEvent(ctx, eventID)
		if err != nil {
			return model.Event{}, err
		}
		if event.OwnerSubject != principal || cohost == "" || cohost == principal {
			return model.Event{}, ErrForbidden
		}
		if event.Cohosts == nil {
			event.Cohosts = map[string][]model.Capability{}
		}
		if len(capabilities) == 0 {
			delete(event.Cohosts, cohost)
		} else {
			event.Cohosts[cohost] = capabilities
		}
		event.UpdatedAt = s.now().UTC()
		if _, err = s.store.Update(ctx, eventKey(eventID), encode(event), revision); errors.Is(err, store.ErrConflict) {
			continue
		} else if err != nil {
			return model.Event{}, err
		}
		return event, nil
	}
	return model.Event{}, ErrConflict
}

func (s *Service) StartSession(ctx context.Context, invitation string) (Session, error) {
	if s.maxCapacity == 0 {
		return Session{}, ErrAdmissionClosed
	}
	index, err := s.store.Get(ctx, invitationKey(s.digest(invitation)))
	if errors.Is(err, store.ErrNotFound) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, err
	}
	event, _, err := s.loadEvent(ctx, string(index.Value))
	if err != nil {
		return Session{}, err
	}
	if event.State != model.EventOpen || event.Capacity == 0 {
		return Session{}, ErrAdmissionClosed
	}
	now := s.now().UTC()
	for number := 1; number <= event.Capacity; number++ {
		secret, secretErr := randomSecret()
		if secretErr != nil {
			return Session{}, secretErr
		}
		generation := uuid.NewString()
		slot := model.Slot{
			EventID: event.ID, Number: number, Generation: generation,
			Subject:      "guest:" + event.ID + ":" + generation,
			ResumeDigest: s.digest(secret), LeaseExpiresAt: now.Add(s.leaseTTL),
			GraceExpiresAt: now.Add(s.leaseTTL + s.gracePeriod), CreatedAt: now, LastSeenAt: now,
		}
		key := slotKey(event.ID, number)
		_, createErr := s.store.Create(ctx, key, encode(slot))
		if errors.Is(createErr, store.ErrExists) {
			existing, revision, loadErr := s.loadSlot(ctx, event.ID, number)
			if loadErr != nil || existing.Reserved || now.Before(existing.GraceExpiresAt) {
				continue
			}
			if s.cleaner == nil {
				continue
			}
			if _, cleanErr := s.cleaner.Cleanup(ctx, uuid.NewString(), event.ApplicationID, event.ID, []string{existing.Subject}, "guest_slot_reclaimed"); cleanErr != nil {
				continue
			}
			if _, createErr = s.store.Update(ctx, key, encode(slot), revision); createErr != nil {
				continue
			}
			_ = s.store.Delete(ctx, sessionKey(existing.ResumeDigest))
		}
		if createErr != nil && !errors.Is(createErr, store.ErrExists) {
			return Session{}, createErr
		}
		currentEvent, _, loadErr := s.loadEvent(ctx, event.ID)
		if loadErr != nil || currentEvent.State != model.EventOpen || number > currentEvent.Capacity {
			_ = s.store.Delete(ctx, key)
			if loadErr != nil {
				return Session{}, loadErr
			}
			return Session{}, ErrAdmissionClosed
		}
		if _, err = s.store.Create(ctx, sessionKey(slot.ResumeDigest), encode(model.SessionIndex{EventID: event.ID, Slot: number})); err != nil {
			_ = s.store.Delete(ctx, key)
			continue
		}
		return s.mint(event, slot, secret)
	}
	return Session{}, ErrCapacity
}

func (s *Service) RenewSession(ctx context.Context, resumeSecret string) (Session, error) {
	event, slot, err := s.renewLease(ctx, resumeSecret)
	if err != nil {
		return Session{}, err
	}
	return s.mint(event, slot, resumeSecret)
}

func (s *Service) HeartbeatSession(ctx context.Context, resumeSecret string) (time.Time, error) {
	_, slot, err := s.renewLease(ctx, resumeSecret)
	if err != nil {
		return time.Time{}, err
	}
	return slot.LeaseExpiresAt, nil
}

func (s *Service) renewLease(ctx context.Context, resumeSecret string) (model.Event, model.Slot, error) {
	digest := s.digest(resumeSecret)
	indexEntry, err := s.store.Get(ctx, sessionKey(digest))
	if errors.Is(err, store.ErrNotFound) {
		return model.Event{}, model.Slot{}, ErrSessionExpired
	}
	if err != nil {
		return model.Event{}, model.Slot{}, err
	}
	var index model.SessionIndex
	if err = json.Unmarshal(indexEntry.Value, &index); err != nil {
		return model.Event{}, model.Slot{}, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		event, _, err := s.loadEvent(ctx, index.EventID)
		if err != nil {
			return model.Event{}, model.Slot{}, err
		}
		if event.State != model.EventOpen {
			return model.Event{}, model.Slot{}, ErrAdmissionClosed
		}
		slot, revision, loadErr := s.loadSlot(ctx, index.EventID, index.Slot)
		if loadErr != nil || !hmac.Equal([]byte(slot.ResumeDigest), []byte(digest)) {
			return model.Event{}, model.Slot{}, ErrSessionExpired
		}
		now := s.now().UTC()
		if !slot.Reserved && now.After(slot.GraceExpiresAt) {
			return model.Event{}, model.Slot{}, ErrSessionExpired
		}
		slot.LastSeenAt = now
		slot.Reserved = false
		slot.LeaseExpiresAt = now.Add(s.leaseTTL)
		slot.GraceExpiresAt = slot.LeaseExpiresAt.Add(s.gracePeriod)
		if _, err = s.store.Update(ctx, slotKey(index.EventID, index.Slot), encode(slot), revision); errors.Is(err, store.ErrConflict) {
			continue
		} else if err != nil {
			return model.Event{}, model.Slot{}, err
		}
		return event, slot, nil
	}
	return model.Event{}, model.Slot{}, ErrConflict
}

func (s *Service) ListSlots(ctx context.Context, principal, eventID string) ([]model.Slot, error) {
	event, err := s.GetEvent(ctx, principal, eventID)
	if err != nil {
		return nil, err
	}
	if !can(event, principal, model.CapabilityInspect) && !can(event, principal, model.CapabilityOperate) {
		return nil, ErrForbidden
	}
	entries, err := s.store.List(ctx, "slot."+eventID+".")
	if err != nil {
		return nil, err
	}
	result := make([]model.Slot, 0, len(entries))
	for _, entry := range entries {
		var slot model.Slot
		if json.Unmarshal(entry.Value, &slot) == nil {
			result = append(result, slot)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func (s *Service) ReleaseSlot(ctx context.Context, principal, eventID string, number int) error {
	event, err := s.GetEvent(ctx, principal, eventID)
	if err != nil {
		return err
	}
	if !can(event, principal, model.CapabilityOperate) {
		return ErrForbidden
	}
	slot, _, err := s.loadSlot(ctx, eventID, number)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if s.cleaner == nil {
		return errors.New("cleanup is not configured")
	}
	if _, err := s.cleaner.Cleanup(ctx, uuid.NewString(), event.ApplicationID, event.ID, []string{slot.Subject}, "guest_slot_released"); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, slotKey(eventID, number)); err != nil {
		return err
	}
	_ = s.store.Delete(ctx, sessionKey(slot.ResumeDigest))
	return nil
}

func (s *Service) DeleteEvent(ctx context.Context, principal, eventID string) (model.DeletionReceipt, error) {
	event, revision, err := s.loadEvent(ctx, eventID)
	if errors.Is(err, ErrNotFound) {
		entry, receiptErr := s.store.Get(ctx, "receipt."+eventID)
		if receiptErr != nil {
			return model.DeletionReceipt{}, ErrNotFound
		}
		var receipt model.DeletionReceipt
		if json.Unmarshal(entry.Value, &receipt) != nil {
			return model.DeletionReceipt{}, ErrNotFound
		}
		if receipt.OwnerSubject != principal && receipt.DeletedBySubject != principal {
			return model.DeletionReceipt{}, ErrForbidden
		}
		return receipt, nil
	}
	if err != nil {
		return model.DeletionReceipt{}, err
	}
	if !can(event, principal, model.CapabilityDelete) {
		return model.DeletionReceipt{}, ErrForbidden
	}
	if s.cleaner == nil {
		return model.DeletionReceipt{}, fmt.Errorf("cleanup is not configured")
	}
	if event.State != model.EventDeleting {
		event.State = model.EventDeleting
		event.DeletionID = uuid.NewString()
		event.UpdatedAt = s.now().UTC()
		if _, err = s.store.Update(ctx, eventKey(eventID), encode(event), revision); err != nil {
			return model.DeletionReceipt{}, err
		}
	}
	entries, err := s.store.List(ctx, "slot."+eventID+".")
	if err != nil {
		return model.DeletionReceipt{}, err
	}
	subjects := make([]string, 0, len(entries))
	for _, entry := range entries {
		var slot model.Slot
		if json.Unmarshal(entry.Value, &slot) == nil {
			subjects = append(subjects, slot.Subject)
		}
	}
	processed, err := s.cleaner.Cleanup(ctx, event.DeletionID, event.ApplicationID, event.ID, subjects, "demo_event_deleted")
	if err != nil {
		return model.DeletionReceipt{}, err
	}
	receipt := model.DeletionReceipt{
		DeletionID: event.DeletionID,
		EventID:    event.ID, ApplicationID: event.ApplicationID, OwnerSubject: event.OwnerSubject,
		DeletedBySubject: principal, CreatedAt: event.CreatedAt, DeletedAt: s.now().UTC(), IdentitiesDeleted: processed,
	}
	if _, err = s.store.Create(ctx, "receipt."+event.ID, encode(receipt)); err != nil && !errors.Is(err, store.ErrExists) {
		return model.DeletionReceipt{}, err
	}
	for key, entry := range entries {
		var slot model.Slot
		if json.Unmarshal(entry.Value, &slot) == nil {
			if err := deleteIfExists(ctx, s.store, sessionKey(slot.ResumeDigest)); err != nil {
				return model.DeletionReceipt{}, err
			}
		}
		if err := deleteIfExists(ctx, s.store, key); err != nil {
			return model.DeletionReceipt{}, err
		}
	}
	if err := deleteIfExists(ctx, s.store, invitationKey(event.InvitationDigest)); err != nil {
		return model.DeletionReceipt{}, err
	}
	if err := deleteIfExists(ctx, s.store, eventKey(event.ID)); err != nil {
		return model.DeletionReceipt{}, err
	}
	return receipt, nil
}

func (s *Service) mint(event model.Event, slot model.Slot, resumeSecret string) (Session, error) {
	display := "Demo User " + strconv.Itoa(slot.Number)
	token, expires, err := s.issuer.Mint(issuer.Guest{
		Subject: slot.Subject, Name: display, Username: "demo-user-" + strconv.Itoa(slot.Number), ApplicationID: event.ApplicationID,
		EventID: event.ID, Slot: slot.Number,
	})
	if err != nil {
		return Session{}, err
	}
	return Session{
		EventID: event.ID, Subject: slot.Subject, DisplayName: display, Slot: slot.Number,
		Token: token, TokenExpiresAt: expires, LeaseExpiresAt: slot.LeaseExpiresAt, ResumeSecret: resumeSecret,
	}, nil
}

func (s *Service) loadEvent(ctx context.Context, id string) (model.Event, uint64, error) {
	entry, err := s.store.Get(ctx, eventKey(id))
	if errors.Is(err, store.ErrNotFound) {
		return model.Event{}, 0, ErrNotFound
	}
	if err != nil {
		return model.Event{}, 0, err
	}
	var event model.Event
	if err := json.Unmarshal(entry.Value, &event); err != nil {
		return model.Event{}, 0, err
	}
	return event, entry.Revision, nil
}

func (s *Service) loadSlot(ctx context.Context, eventID string, number int) (model.Slot, uint64, error) {
	entry, err := s.store.Get(ctx, slotKey(eventID, number))
	if err != nil {
		return model.Slot{}, 0, err
	}
	var slot model.Slot
	if err := json.Unmarshal(entry.Value, &slot); err != nil {
		return model.Slot{}, 0, err
	}
	return slot, entry.Revision, nil
}

func (s *Service) reserveSlots(ctx context.Context, eventID string) error {
	entries, err := s.store.List(ctx, "slot."+eventID+".")
	if err != nil {
		return err
	}
	for key, entry := range entries {
		var slot model.Slot
		if err := json.Unmarshal(entry.Value, &slot); err != nil {
			return err
		}
		if slot.Reserved {
			continue
		}
		slot.Reserved = true
		if _, err := s.store.Update(ctx, key, encode(slot), entry.Revision); errors.Is(err, store.ErrConflict) {
			return s.reserveSlots(ctx, eventID)
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) digest(value string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) invitationURL(secret string) string { return s.publicDemoURL + "#" + secret }

func randomSecret() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func encode(value interface{}) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func can(event model.Event, principal string, capability model.Capability) bool {
	if event.OwnerSubject == principal {
		return true
	}
	for _, granted := range event.Cohosts[principal] {
		if granted == capability {
			return true
		}
	}
	return false
}

func validTransition(from model.EventState, to model.EventState) bool {
	return (from == model.EventDraft && to == model.EventOpen) ||
		(from == model.EventOpen && to == model.EventPaused) ||
		(from == model.EventPaused && to == model.EventOpen)
}

func deleteIfExists(ctx context.Context, st store.Store, key string) error {
	err := st.Delete(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

func eventKey(id string) string          { return "event." + id }
func invitationKey(digest string) string { return "invitation." + digest }
func sessionKey(digest string) string    { return "session." + digest }
func slotKey(eventID string, number int) string {
	return fmt.Sprintf("slot.%s.%03d", eventID, number)
}
