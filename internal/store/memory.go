package store

import (
	"context"
	"strings"
	"sync"
)

type Memory struct {
	mu       sync.RWMutex
	revision uint64
	entries  map[string]Entry
}

func NewMemory() *Memory {
	return &Memory{entries: make(map[string]Entry)}
}

func (m *Memory) Get(_ context.Context, key string) (Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[key]
	if !ok {
		return Entry{}, ErrNotFound
	}
	entry.Value = append([]byte(nil), entry.Value...)
	return entry, nil
}

func (m *Memory) Create(_ context.Context, key string, value []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; ok {
		return 0, ErrExists
	}
	m.revision++
	m.entries[key] = Entry{Value: append([]byte(nil), value...), Revision: m.revision}
	return m.revision, nil
}

func (m *Memory) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok {
		return 0, ErrNotFound
	}
	if entry.Revision != revision {
		return 0, ErrConflict
	}
	m.revision++
	m.entries[key] = Entry{Value: append([]byte(nil), value...), Revision: m.revision}
	return m.revision, nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; !ok {
		return ErrNotFound
	}
	delete(m.entries, key)
	return nil
}

func (m *Memory) List(_ context.Context, prefix string) (map[string]Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]Entry)
	for key, entry := range m.entries {
		if strings.HasPrefix(key, prefix) {
			entry.Value = append([]byte(nil), entry.Value...)
			result[key] = entry
		}
	}
	return result, nil
}
