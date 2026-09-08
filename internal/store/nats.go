package store

import (
	"context"
	"errors"
	"strings"

	"github.com/nats-io/nats.go/jetstream"
)

type NATS struct {
	kv jetstream.KeyValue
}

func NewNATS(ctx context.Context, js jetstream.JetStream, bucket string) (*NATS, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if errors.Is(err, jetstream.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:      bucket,
			Description: "Anonymous guest events, slots, and session indexes",
			History:     1,
			Storage:     jetstream.FileStorage,
		})
	}
	if err != nil {
		return nil, err
	}
	return &NATS{kv: kv}, nil
}

func (n *NATS) Get(ctx context.Context, key string) (Entry, error) {
	entry, err := n.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return Entry{}, ErrNotFound
	}
	if err != nil {
		return Entry{}, err
	}
	return Entry{Value: append([]byte(nil), entry.Value()...), Revision: entry.Revision()}, nil
}

func (n *NATS) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	revision, err := n.kv.Create(ctx, key, value)
	if errors.Is(err, jetstream.ErrKeyExists) {
		return 0, ErrExists
	}
	return revision, err
}

func (n *NATS) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	next, err := n.kv.Update(ctx, key, value, revision)
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		return 0, ErrConflict
	}
	if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
		return 0, ErrNotFound
	}
	return next, err
}

func (n *NATS) Delete(ctx context.Context, key string) error {
	err := n.kv.Purge(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return ErrNotFound
	}
	return err
}

func (n *NATS) List(ctx context.Context, prefix string) (map[string]Entry, error) {
	lister, err := n.kv.ListKeysFiltered(ctx, prefix+">")
	if errors.Is(err, jetstream.ErrNoKeysFound) {
		return map[string]Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]Entry)
	for key := range lister.Keys() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, getErr := n.Get(ctx, key)
		if errors.Is(getErr, ErrNotFound) {
			continue
		}
		if getErr != nil {
			return nil, getErr
		}
		result[key] = entry
	}
	return result, nil
}
