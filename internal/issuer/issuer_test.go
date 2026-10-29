package issuer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestGuestTokenIsAcceptedByCoreOSOIDCVerifier(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var tokenIssuer *Issuer
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	tokenIssuer, err = New(key, server.URL, "whengas-demo", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenIssuer.Discovery())
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenIssuer.JWKS())
	})

	raw, _, err := tokenIssuer.Mint(Guest{
		Subject: "guest:event:generation", Name: "Demo User 1", Username: "demo-user-1",
		ApplicationID: "whengas", EventID: "event", Slot: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := oidc.NewProvider(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: "whengas-demo"}).Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("verification failed: %v", err)
	}
	var claims struct {
		Guest         bool   `json:"guest"`
		ApplicationID string `json:"application_id"`
		EventID       string `json:"event_id"`
		Slot          int    `json:"slot"`
	}
	if err := verified.Claims(&claims); err != nil {
		t.Fatal(err)
	}
	if !claims.Guest || claims.ApplicationID != "whengas" || claims.EventID != "event" || claims.Slot != 1 {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}
