package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/archilea/rein/internal/meter"
)

func TestOpenSpendMeter_MemoryURL(t *testing.T) {
	m, err := openSpendMeter("memory")
	if err != nil {
		t.Fatalf("openSpendMeter(memory): %v", err)
	}
	if _, ok := m.(*meter.Memory); !ok {
		t.Errorf("want *meter.Memory, got %T", m)
	}
}

func TestOpenSpendMeter_EmptyURLUsesMemory(t *testing.T) {
	m, err := openSpendMeter("")
	if err != nil {
		t.Fatalf("openSpendMeter(empty): %v", err)
	}
	if _, ok := m.(*meter.Memory); !ok {
		t.Errorf("want *meter.Memory, got %T", m)
	}
}

func TestOpenSpendMeter_SQLiteURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wiring.db")
	m, err := openSpendMeter("sqlite:" + path)
	if err != nil {
		t.Fatalf("openSpendMeter(sqlite:...): %v", err)
	}
	sq, ok := m.(*meter.SQLite)
	if !ok {
		t.Fatalf("want *meter.SQLite, got %T", m)
	}
	t.Cleanup(func() { _ = sq.Close() })

	// Smoke: the meter is functional.
	if err := sq.Record(context.Background(), "k", 1.0); err != nil {
		t.Errorf("Record: %v", err)
	}
}

func TestOpenSpendMeter_UnsupportedSchemeErrors(t *testing.T) {
	if _, err := openSpendMeter("postgres://localhost"); err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

func TestOpenSpendMeter_MemoryColonURL(t *testing.T) {
	m, err := openSpendMeter("memory:")
	if err != nil {
		t.Fatalf("openSpendMeter(memory:): %v", err)
	}
	if _, ok := m.(*meter.Memory); !ok {
		t.Errorf("want *meter.Memory, got %T", m)
	}
}
