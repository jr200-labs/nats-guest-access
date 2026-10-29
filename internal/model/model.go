package model

import "time"

type EventState string

const (
	EventDraft    EventState = "draft"
	EventOpen     EventState = "open"
	EventPaused   EventState = "paused"
	EventDeleting EventState = "deleting"
	EventDeleted  EventState = "deleted"
)

type Event struct {
	Cleanup               *CleanupProgress        `json:"cleanup,omitempty"`
	DeletedBySubject      string                  `json:"deleted_by_subject,omitempty"`
	ID                    string                  `json:"id"`
	ApplicationID         string                  `json:"application_id"`
	Name                  string                  `json:"name"`
	OwnerSubject          string                  `json:"owner_subject"`
	Capacity              int                     `json:"capacity"`
	InspectLiveWorkspaces bool                    `json:"inspect_live_workspaces"`
	State                 EventState              `json:"state"`
	InvitationDigest      string                  `json:"invitation_digest"`
	DeletionID            string                  `json:"deletion_id,omitempty"`
	Cohosts               map[string][]Capability `json:"cohosts,omitempty"`
	CreatedAt             time.Time               `json:"created_at"`
	UpdatedAt             time.Time               `json:"updated_at"`
}

type Capability string

const (
	CapabilityOperate Capability = "operate"
	CapabilityInspect Capability = "inspect"
	CapabilityDelete  Capability = "delete"
)

type Slot struct {
	Cleanup        *CleanupProgress `json:"cleanup,omitempty"`
	EventID        string           `json:"event_id"`
	Number         int              `json:"number"`
	Generation     string           `json:"generation"`
	Subject        string           `json:"subject"`
	ResumeDigest   string           `json:"resume_digest"`
	Reserved       bool             `json:"reserved"`
	LeaseExpiresAt time.Time        `json:"lease_expires_at"`
	GraceExpiresAt time.Time        `json:"grace_expires_at"`
	CreatedAt      time.Time        `json:"created_at"`
	LastSeenAt     time.Time        `json:"last_seen_at"`
}

type SessionIndex struct {
	Deleted bool   `json:"deleted,omitempty"`
	EventID string `json:"event_id"`
	Slot    int    `json:"slot"`
}

type DeletionReceipt struct {
	DeletionID        string    `json:"deletion_id"`
	EventID           string    `json:"event_id"`
	ApplicationID     string    `json:"application_id"`
	OwnerSubject      string    `json:"owner_subject"`
	DeletedBySubject  string    `json:"deleted_by_subject"`
	CreatedAt         time.Time `json:"created_at"`
	DeletedAt         time.Time `json:"deleted_at"`
	IdentitiesDeleted int       `json:"identities_deleted"`
}

// CleanupProgress is durable retry state. LastError is a bounded code, never a
// downstream error string (which could contain credentials or user content).
type CleanupProgress struct {
	ID            string    `json:"id"`
	Attempts      int       `json:"attempts"`
	StartedAt     time.Time `json:"started_at"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
	LastError     string    `json:"last_error,omitempty"`
}
