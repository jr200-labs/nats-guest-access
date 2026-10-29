package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jr200-labs/nats-guest-access/internal/access"
	"github.com/jr200-labs/nats-guest-access/internal/api"
	"github.com/jr200-labs/nats-guest-access/internal/cleanup"
	"github.com/jr200-labs/nats-guest-access/internal/config"
	"github.com/jr200-labs/nats-guest-access/internal/issuer"
	"github.com/jr200-labs/nats-guest-access/internal/store"
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "keygen" {
		if err := keygen(os.Args[2]); err != nil {
			slog.Error("key generation failed", "error", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: nats-guest-access [keygen <output.pem>]")
		os.Exit(2)
	}
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	tokenIssuer, err := issuer.Load(cfg.SigningKeyFile, cfg.IssuerURL, cfg.Audience, cfg.TokenTTL)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	natsOptions := []nats.Option{nats.Name("nats-guest-access")}
	if cfg.NATSCredentialsFile != "" {
		natsOptions = append(natsOptions, nats.UserCredentials(cfg.NATSCredentialsFile))
	}
	nc, err := nats.Connect(cfg.NATSURL, natsOptions...)
	if err != nil {
		return fmt.Errorf("connect to NATS: %w", err)
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("open JetStream: %w", err)
	}
	repository, err := store.NewNATS(ctx, js, cfg.KVBucket)
	if err != nil {
		return fmt.Errorf("open state bucket: %w", err)
	}
	presenters, err := api.NewPresenterVerifier(ctx, cfg.PresenterIssuerURL, cfg.PresenterClientID, cfg.PresenterGroup)
	if err != nil {
		return fmt.Errorf("configure presenter verifier: %w", err)
	}
	cleaner := cleanup.NewNATS(nc, cfg.CleanupSubject, cfg.CleanupTimeout)
	service := access.New(repository, tokenIssuer, cfg.InvitationHMACKey, cfg.PublicDemoURL, cfg.MaxCapacity, cfg.LeaseTTL, cfg.GracePeriod, cleaner)
	server := &http.Server{
		Addr: cfg.ListenAddress, Handler: api.New(service, tokenIssuer, presenters, cfg.SecureCookies),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "address", cfg.ListenAddress, "issuer", cfg.IssuerURL)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func keygen(path string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	data, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: data}), 0o600)
}
