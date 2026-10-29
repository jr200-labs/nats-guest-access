package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type NATS struct {
	connection *nats.Conn
	subject    string
	timeout    time.Duration
}

type Request struct {
	SchemaVersion int      `json:"schema_version"`
	DeletionID    string   `json:"deletion_id"`
	ApplicationID string   `json:"application_id"`
	EventID       string   `json:"event_id"`
	GuestSubjects []string `json:"guest_subjects"`
	Reason        string   `json:"reason"`
}

type Response struct {
	SchemaVersion       int    `json:"schema_version"`
	DeletionID          string `json:"deletion_id"`
	ApplicationID       string `json:"application_id"`
	Status              string `json:"status"`
	IdentitiesProcessed int    `json:"identities_processed"`
	Error               string `json:"error,omitempty"`
}

func NewNATS(connection *nats.Conn, subject string, timeout time.Duration) *NATS {
	if subject == "" {
		return nil
	}
	return &NATS{connection: connection, subject: subject, timeout: timeout}
}

func (n *NATS) Cleanup(ctx context.Context, deletionID, applicationID, eventID string, subjects []string, reason string) (int, error) {
	if deletionID == "" {
		deletionID = uuid.NewString()
	}
	payload, err := json.Marshal(Request{
		SchemaVersion: 1, DeletionID: deletionID, ApplicationID: applicationID,
		EventID: eventID, GuestSubjects: subjects, Reason: reason,
	})
	if err != nil {
		return 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()
	message, err := n.connection.RequestWithContext(requestCtx, n.subject+"."+applicationID, payload)
	if err != nil {
		return 0, fmt.Errorf("request application cleanup: %w", err)
	}
	var response Response
	if err := json.Unmarshal(message.Data, &response); err != nil {
		return 0, fmt.Errorf("decode application cleanup response: %w", err)
	}
	if response.SchemaVersion != 1 || response.DeletionID != deletionID || response.ApplicationID != applicationID {
		return 0, errors.New("application cleanup response correlation mismatch")
	}
	if response.Status != "complete" {
		return 0, fmt.Errorf("application cleanup failed: %s", response.Error)
	}
	if response.IdentitiesProcessed < 0 {
		return 0, errors.New("application cleanup returned a negative identity count")
	}
	return response.IdentitiesProcessed, nil
}
