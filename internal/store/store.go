package store

import (
	"context"
	"errors"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("revision conflict")
	ErrExists   = errors.New("already exists")
)

type Entry struct {
	Value    []byte
	Revision uint64
}

type Store interface {
	Get(context.Context, string) (Entry, error)
	Create(context.Context, string, []byte) (uint64, error)
	Update(context.Context, string, []byte, uint64) (uint64, error)
	Delete(context.Context, string) error
	List(context.Context, string) (map[string]Entry, error)
}
