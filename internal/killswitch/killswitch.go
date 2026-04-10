// Package killswitch implements Rein's global freeze.
//
// A kill-switch is a single boolean that, when true, causes the proxy to
// reject every outbound request with 503 Service Unavailable. It is the
// simplest possible incident-response primitive: one state, two commands.
//
// The switch is deliberately global and not per-key. Per-key revocation is a
// different concept and lives in the keys package.
package killswitch

import (
	"context"
	"sync/atomic"
)

// Switch represents the global freeze state.
// Implementations must be safe for concurrent use.
type Switch interface {
	IsFrozen(ctx context.Context) (bool, error)
	SetFrozen(ctx context.Context, frozen bool) error
}

// Memory is an in-memory Switch backed by an atomic.Bool.
// State is lost on process restart.
type Memory struct {
	frozen atomic.Bool
}

// NewMemory returns a new in-memory kill-switch in the unfrozen state.
func NewMemory() *Memory {
	return &Memory{}
}

// IsFrozen reports the current freeze state.
func (m *Memory) IsFrozen(_ context.Context) (bool, error) {
	return m.frozen.Load(), nil
}

// SetFrozen updates the freeze state.
func (m *Memory) SetFrozen(_ context.Context, frozen bool) error {
	m.frozen.Store(frozen)
	return nil
}
