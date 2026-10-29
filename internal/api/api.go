package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jr200-labs/nats-guest-access/internal/access"
	"github.com/jr200-labs/nats-guest-access/internal/issuer"
	"github.com/jr200-labs/nats-guest-access/internal/model"
)

const resumeCookie = "nats_guest_access"

type API struct {
	access        *access.Service
	issuer        *issuer.Issuer
	presenters    *PresenterVerifier
	secureCookies bool
}

type eventResponse struct {
	ID                    string                        `json:"id"`
	ApplicationID         string                        `json:"application_id"`
	Name                  string                        `json:"name"`
	OwnerSubject          string                        `json:"owner_subject"`
	Capacity              int                           `json:"capacity"`
	InspectLiveWorkspaces bool                          `json:"inspect_live_workspaces"`
	State                 model.EventState              `json:"state"`
	Cohosts               map[string][]model.Capability `json:"cohosts,omitempty"`
	CreatedAt             time.Time                     `json:"created_at"`
	UpdatedAt             time.Time                     `json:"updated_at"`
}

type slotResponse struct {
	Number         int       `json:"number"`
	Subject        string    `json:"subject"`
	Reserved       bool      `json:"reserved"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
	GraceExpiresAt time.Time `json:"grace_expires_at"`
	CreatedAt      time.Time `json:"created_at"`
	LastSeenAt     time.Time `json:"last_seen_at"`
}

func New(service *access.Service, tokenIssuer *issuer.Issuer, presenters *PresenterVerifier, secureCookies bool) http.Handler {
	api := &API{access: service, issuer: tokenIssuer, presenters: presenters, secureCookies: secureCookies}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", api.discovery)
	mux.HandleFunc("GET /jwks", api.jwks)
	mux.HandleFunc("GET /healthz", api.health)
	mux.HandleFunc("GET /readyz", api.health)
	mux.HandleFunc("POST /v1/guest-sessions", api.startSession)
	mux.HandleFunc("POST /v1/guest-sessions/token", api.renewSession)
	mux.HandleFunc("POST /v1/guest-sessions/heartbeat", api.heartbeatSession)
	mux.HandleFunc("DELETE /v1/guest-sessions/current", api.endSession)
	mux.HandleFunc("/v1/events", api.events)
	mux.HandleFunc("/v1/events/", api.event)
	return securityHeaders(requestLog(bodyLimit(mux)))
}

func (a *API) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.issuer.Discovery())
}
func (a *API) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.issuer.JWKS())
}
func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) startSession(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Invitation string `json:"invitation"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var session access.Session
	var err error
	if input.Invitation != "" {
		session, err = a.access.StartSession(r.Context(), input.Invitation)
	} else if cookie, cookieErr := r.Cookie(resumeCookie); cookieErr == nil {
		session, err = a.access.RenewSession(r.Context(), cookie.Value)
	} else {
		err = access.ErrNotFound
	}
	if err != nil {
		a.writeAccessError(w, err)
		return
	}
	a.setResumeCookie(w, session.ResumeSecret)
	writeJSON(w, http.StatusCreated, session)
}

func (a *API) renewSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(resumeCookie)
	if err != nil {
		a.writeAccessError(w, access.ErrSessionExpired)
		return
	}
	session, err := a.access.RenewSession(r.Context(), cookie.Value)
	if err != nil {
		a.writeAccessError(w, err)
		return
	}
	a.setResumeCookie(w, session.ResumeSecret)
	writeJSON(w, http.StatusOK, session)
}

func (a *API) heartbeatSession(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(resumeCookie)
	if err != nil {
		a.writeAccessError(w, access.ErrSessionExpired)
		return
	}
	expires, err := a.access.HeartbeatSession(r.Context(), cookie.Value)
	if err != nil {
		a.writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]time.Time{"lease_expires_at": expires})
}

func (a *API) endSession(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: resumeCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secureCookies,
		SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0),
	})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	principal, err := a.presenters.Verify(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "presenter authentication failed")
		return
	}
	switch r.Method {
	case http.MethodPost:
		var input access.CreateEventInput
		if err := decodeJSON(r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		created, err := a.access.CreateEvent(r.Context(), principal, input)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"event": eventView(created.Event), "invitation_url": created.InvitationURL,
		})
	case http.MethodGet:
		events, err := a.access.ListEvents(r.Context(), principal)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		views := make([]eventResponse, 0, len(events))
		for _, event := range events {
			views = append(views, eventView(event))
		}
		writeJSON(w, http.StatusOK, views)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (a *API) event(w http.ResponseWriter, r *http.Request) {
	principal, err := a.presenters.Verify(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "presenter authentication failed")
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/events/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "not_found", "event not found")
		return
	}
	eventID := parts[0]
	if len(parts) == 3 && parts[1] == "slots" && r.Method == http.MethodDelete {
		number, parseErr := strconv.Atoi(parts[2])
		if parseErr != nil || number < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "slot number is invalid")
			return
		}
		if err := a.access.ReleaseSlot(r.Context(), principal, eventID, number); err != nil {
			a.writeAccessError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		event, err := a.access.GetEvent(r.Context(), principal, eventID)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventView(event))
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	switch parts[1] {
	case "open", "pause", "reopen":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		target := model.EventOpen
		if parts[1] == "pause" {
			target = model.EventPaused
		}
		event, err := a.access.SetState(r.Context(), principal, eventID, target)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventView(event))
	case "invitation":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		url, err := a.access.RotateInvitation(r.Context(), principal, eventID)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"invitation_url": url})
	case "capacity":
		var input struct {
			Capacity int `json:"capacity"`
		}
		if r.Method != http.MethodPut || decodeJSON(r, &input) != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "capacity is required")
			return
		}
		event, err := a.access.UpdateCapacity(r.Context(), principal, eventID, input.Capacity)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventView(event))
	case "cohosts":
		var input struct {
			Subject      string             `json:"subject"`
			Capabilities []model.Capability `json:"capabilities"`
		}
		if r.Method != http.MethodPut || decodeJSON(r, &input) != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "cohost is invalid")
			return
		}
		event, err := a.access.SetCohost(r.Context(), principal, eventID, input.Subject, input.Capabilities)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, eventView(event))
	case "slots":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		slots, err := a.access.ListSlots(r.Context(), principal, eventID)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		views := make([]slotResponse, 0, len(slots))
		for _, slot := range slots {
			views = append(views, slotView(slot))
		}
		writeJSON(w, http.StatusOK, views)
	case "delete":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		receipt, err := a.access.DeleteEvent(r.Context(), principal, eventID)
		if err != nil {
			a.writeAccessError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	default:
		writeError(w, http.StatusNotFound, "not_found", "operation not found")
	}
}

func (a *API) setResumeCookie(w http.ResponseWriter, secret string) {
	http.SetCookie(w, &http.Cookie{
		Name: resumeCookie, Value: secret, Path: "/", HttpOnly: true,
		Secure: a.secureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) writeAccessError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal_error"
	switch {
	case errors.Is(err, access.ErrNotFound), errors.Is(err, access.ErrSessionExpired):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, access.ErrForbidden):
		status, code = http.StatusForbidden, "forbidden"
	case errors.Is(err, access.ErrConflict):
		status, code = http.StatusConflict, "conflict"
	case errors.Is(err, access.ErrCapacity):
		status, code = http.StatusTooManyRequests, "capacity_reached"
	case errors.Is(err, access.ErrAdmissionClosed):
		status, code = http.StatusServiceUnavailable, "admission_closed"
	}
	if status == http.StatusInternalServerError {
		slog.Error("request failed", "error", err)
		writeError(w, status, code, "request failed")
		return
	}
	writeError(w, status, code, err.Error())
}

func decodeJSON(r *http.Request, target interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func eventView(event model.Event) eventResponse {
	return eventResponse{
		ID: event.ID, ApplicationID: event.ApplicationID, Name: event.Name,
		OwnerSubject: event.OwnerSubject, Capacity: event.Capacity,
		InspectLiveWorkspaces: event.InspectLiveWorkspaces, State: event.State,
		Cohosts: event.Cohosts, CreatedAt: event.CreatedAt, UpdatedAt: event.UpdatedAt,
	}
}

func slotView(slot model.Slot) slotResponse {
	return slotResponse{
		Number: slot.Number, Subject: slot.Subject, Reserved: slot.Reserved,
		LeaseExpiresAt: slot.LeaseExpiresAt, GraceExpiresAt: slot.GraceExpiresAt,
		CreatedAt: slot.CreatedAt, LastSeenAt: slot.LastSeenAt,
	}
}

func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", strconv.FormatInt(time.Since(started).Milliseconds(), 10))
	})
}
