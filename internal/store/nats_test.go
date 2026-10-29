package store

import (
	"context"
	"errors"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func TestNATSStoreCASAndListing(t *testing.T) {
	server, err := natsserver.NewServer(&natsserver.Options{
		JetStream: true, StoreDir: t.TempDir(), Port: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Start()
	t.Cleanup(server.Shutdown)
	if !server.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	nc, err := nats.Connect(server.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewNATS(context.Background(), js, "TEST_GUEST_ACCESS")
	if err != nil {
		t.Fatal(err)
	}

	revision, err := repository.Create(context.Background(), "event.one", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Create(context.Background(), "event.one", []byte("duplicate")); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create: got %v", err)
	}
	if _, err := repository.Update(context.Background(), "event.one", []byte("two"), revision+1); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: got %v", err)
	}
	if _, err := repository.Update(context.Background(), "event.one", []byte("two"), revision); err != nil {
		t.Fatal(err)
	}
	entries, err := repository.List(context.Background(), "event.")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || string(entries["event.one"].Value) != "two" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
	if err := repository.Delete(context.Background(), "event.one"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Get(context.Background(), "event.one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted key: got %v", err)
	}
}
