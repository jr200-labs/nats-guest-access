package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddress       string
	IssuerURL           string
	Audience            string
	PublicDemoURL       string
	NATSURL             string
	NATSCredentialsFile string
	KVBucket            string
	CleanupSubject      string
	CleanupTimeout      time.Duration
	SigningKeyFile      string
	InvitationHMACKey   string
	TokenTTL            time.Duration
	LeaseTTL            time.Duration
	GracePeriod         time.Duration
	MaxCapacity         int
	PresenterIssuerURL  string
	PresenterClientID   string
	PresenterGroup      string
	SecureCookies       bool
}

func Load() (Config, error) {
	c := Config{
		ListenAddress:       env("NGA_LISTEN_ADDRESS", ":8080"),
		IssuerURL:           strings.TrimRight(os.Getenv("NGA_ISSUER_URL"), "/"),
		Audience:            env("NGA_AUDIENCE", "whengas-demo"),
		PublicDemoURL:       strings.TrimRight(env("NGA_PUBLIC_DEMO_URL", "https://www.whengas.com/demo"), "/"),
		NATSURL:             env("NGA_NATS_URL", "nats://127.0.0.1:4222"),
		NATSCredentialsFile: os.Getenv("NGA_NATS_CREDS_FILE"),
		KVBucket:            env("NGA_KV_BUCKET", "NATS_GUEST_ACCESS"),
		CleanupSubject:      os.Getenv("NGA_CLEANUP_SUBJECT"),
		SigningKeyFile:      os.Getenv("NGA_SIGNING_KEY_FILE"),
		InvitationHMACKey:   os.Getenv("NGA_INVITATION_HMAC_KEY"),
		PresenterIssuerURL:  strings.TrimRight(os.Getenv("NGA_PRESENTER_ISSUER_URL"), "/"),
		PresenterClientID:   os.Getenv("NGA_PRESENTER_CLIENT_ID"),
		PresenterGroup:      env("NGA_PRESENTER_GROUP", "demo-presenters"),
	}

	var err error
	if c.TokenTTL, err = duration("NGA_TOKEN_TTL", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if c.LeaseTTL, err = duration("NGA_LEASE_TTL", 30*time.Second); err != nil {
		return Config{}, err
	}
	if c.GracePeriod, err = duration("NGA_GRACE_PERIOD", 5*time.Minute); err != nil {
		return Config{}, err
	}
	if c.CleanupTimeout, err = duration("NGA_CLEANUP_TIMEOUT", 30*time.Second); err != nil {
		return Config{}, err
	}
	if c.MaxCapacity, err = integer("NGA_MAX_CAPACITY", 100); err != nil {
		return Config{}, err
	}
	if c.SecureCookies, err = boolean("NGA_SECURE_COOKIES", true); err != nil {
		return Config{}, err
	}

	if c.IssuerURL == "" {
		return Config{}, errors.New("NGA_ISSUER_URL is required")
	}
	if c.SigningKeyFile == "" {
		return Config{}, errors.New("NGA_SIGNING_KEY_FILE is required")
	}
	if len(c.InvitationHMACKey) < 32 {
		return Config{}, errors.New("NGA_INVITATION_HMAC_KEY must be at least 32 characters")
	}
	if c.TokenTTL <= 0 || c.TokenTTL > 5*time.Minute {
		return Config{}, errors.New("NGA_TOKEN_TTL must be between zero and five minutes")
	}
	if c.LeaseTTL <= 0 || c.GracePeriod < 0 || c.CleanupTimeout <= 0 || c.MaxCapacity < 0 {
		return Config{}, errors.New("lease TTL must be positive; grace period and capacity cannot be negative")
	}
	if (c.PresenterIssuerURL == "") != (c.PresenterClientID == "") {
		return Config{}, errors.New("NGA_PRESENTER_ISSUER_URL and NGA_PRESENTER_CLIENT_ID must be configured together")
	}
	return c, nil
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func integer(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func boolean(name string, fallback bool) (bool, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}
