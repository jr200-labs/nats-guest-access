package issuer

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
)

type Issuer struct {
	issuerURL string
	audience  string
	ttl       time.Duration
	signer    jose.Signer
	publicJWK jose.JSONWebKey
	now       func() time.Time
}

type Guest struct {
	Subject       string
	Name          string
	Username      string
	ApplicationID string
	EventID       string
	Slot          int
}

type guestClaims struct {
	Guest         bool   `json:"guest"`
	Name          string `json:"name"`
	Username      string `json:"preferred_username"`
	ApplicationID string `json:"application_id"`
	EventID       string `json:"event_id"`
	Slot          int    `json:"slot"`
}

func Load(keyFile, issuerURL, audience string, ttl time.Duration) (*Issuer, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("signing key is not PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if ecKey, ecErr := x509.ParseECPrivateKey(block.Bytes); ecErr == nil {
			key = ecKey
		} else {
			return nil, fmt.Errorf("parse signing key: %w", err)
		}
	}
	privateKey, ok := key.(*ecdsa.PrivateKey)
	if !ok || privateKey.Curve.Params().Name != "P-256" {
		return nil, errors.New("signing key must be an ECDSA P-256 private key")
	}
	return New(privateKey, issuerURL, audience, ttl)
}

func New(privateKey *ecdsa.PrivateKey, issuerURL, audience string, ttl time.Duration) (*Issuer, error) {
	if privateKey == nil || privateKey.Curve.Params().Name != "P-256" {
		return nil, errors.New("signing key must be an ECDSA P-256 private key")
	}
	thumbprint, err := (&jose.JSONWebKey{Key: &privateKey.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("calculate key ID: %w", err)
	}
	kid := fmt.Sprintf("%x", thumbprint[:8])
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: privateKey}, &jose.SignerOptions{ExtraHeaders: map[jose.HeaderKey]interface{}{jose.HeaderKey("kid"): kid}})
	if err != nil {
		return nil, fmt.Errorf("create signer: %w", err)
	}
	return &Issuer{
		issuerURL: issuerURL,
		audience:  audience,
		ttl:       ttl,
		signer:    signer,
		publicJWK: jose.JSONWebKey{Key: &privateKey.PublicKey, KeyID: kid, Algorithm: string(jose.ES256), Use: "sig"},
		now:       time.Now,
	}, nil
}

func (i *Issuer) Mint(guest Guest) (string, time.Time, error) {
	now := i.now().UTC()
	expires := now.Add(i.ttl)
	claims := jwt.Claims{
		Issuer: i.issuerURL, Subject: guest.Subject, Audience: jwt.Audience{i.audience},
		ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(expires),
	}
	extra := guestClaims{
		Guest:         true,
		Name:          guest.Name,
		Username:      guest.Username,
		ApplicationID: guest.ApplicationID,
		EventID:       guest.EventID,
		Slot:          guest.Slot,
	}
	raw, err := jwt.Signed(i.signer).Claims(claims).Claims(extra).Serialize()
	return raw, expires, err
}

func (i *Issuer) Discovery() map[string]interface{} {
	return map[string]interface{}{
		"issuer":                                i.issuerURL,
		"jwks_uri":                              i.issuerURL + "/jwks",
		"id_token_signing_alg_values_supported": []string{string(jose.ES256)},
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
	}
}

func (i *Issuer) JWKS() jose.JSONWebKeySet {
	return jose.JSONWebKeySet{Keys: []jose.JSONWebKey{i.publicJWK}}
}
