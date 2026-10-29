package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type PresenterVerifier struct {
	verifier *oidc.IDTokenVerifier
	group    string
}

type presenterClaims struct {
	Subject string   `json:"sub"`
	Groups  []string `json:"groups"`
}

func NewPresenterVerifier(ctx context.Context, issuerURL, clientID, group string) (*PresenterVerifier, error) {
	if issuerURL == "" {
		return nil, nil
	}
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	return &PresenterVerifier{verifier: provider.Verifier(&oidc.Config{ClientID: clientID}), group: group}, nil
}

func (v *PresenterVerifier) Verify(r *http.Request) (string, error) {
	if v == nil {
		return "", errors.New("presenter authentication is not configured")
	}
	authorization := r.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return "", errors.New("missing bearer token")
	}
	token, err := v.verifier.Verify(r.Context(), strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer ")))
	if err != nil {
		return "", err
	}
	var claims presenterClaims
	if err := token.Claims(&claims); err != nil || claims.Subject == "" {
		return "", errors.New("invalid presenter claims")
	}
	for _, group := range claims.Groups {
		if group == v.group {
			return claims.Subject, nil
		}
	}
	return "", errors.New("presenter group is required")
}
