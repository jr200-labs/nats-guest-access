package api

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jr200-labs/nats-guest-access/internal/access"
	"github.com/jr200-labs/nats-guest-access/internal/issuer"
	"github.com/jr200-labs/nats-guest-access/internal/model"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

func TestInvitationStartsAndResumesBrowserSession(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tokenIssuer, err := issuer.New(key, "https://guest.example", "whengas-demo", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := access.New(store.NewMemory(), tokenIssuer, strings.Repeat("h", 32), "https://www.whengas.com/demo", 10, 30*time.Second, 5*time.Minute, nil)
	event, err := service.CreateEvent(context.Background(), "presenter", access.CreateEventInput{
		ApplicationID: "whengas", Name: "Talk", Capacity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetState(context.Background(), "presenter", event.Event.ID, model.EventOpen); err != nil {
		t.Fatal(err)
	}
	_, invitation, _ := strings.Cut(event.InvitationURL, "#")
	handler := New(service, tokenIssuer, nil, false)

	body, _ := json.Marshal(map[string]string{"invitation": invitation})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/guest-sessions", bytes.NewReader(body)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("start status %d: %s", recorder.Code, recorder.Body.String())
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != resumeCookie || !cookies[0].HttpOnly {
		t.Fatalf("unexpected cookies: %+v", cookies)
	}
	var first access.Session
	if err := json.Unmarshal(recorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.Token == "" || first.Subject == "" || first.ResumeSecret != "" {
		t.Fatalf("unexpected session response: %+v", first)
	}

	renew := httptest.NewRequest(http.MethodPost, "/v1/guest-sessions/token", http.NoBody)
	renew.AddCookie(cookies[0])
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, renew)
	if recorder.Code != http.StatusOK {
		t.Fatalf("renew status %d: %s", recorder.Code, recorder.Body.String())
	}
	var resumed access.Session
	if err := json.Unmarshal(recorder.Body.Bytes(), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Subject != first.Subject || resumed.Slot != first.Slot {
		t.Fatalf("resumed a different session: %+v", resumed)
	}
}
