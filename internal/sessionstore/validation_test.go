package sessionstore

import (
	"context"
	"testing"
)

func TestValidateOptions(t *testing.T) {
	store := NewInMemoryStore()

	// nil store: always ok.
	if err := ValidateOptions(nil, true, false, true); err != nil {
		t.Fatalf("nil store should be ok, got %v", err)
	}

	// store + file checkpointing: error.
	if err := ValidateOptions(store, false, false, true); err == nil {
		t.Fatal("expected error for store + file checkpointing")
	}

	// continue without resume, store implements SessionLister (InMemoryStore does): ok.
	if err := ValidateOptions(store, true, false, false); err != nil {
		t.Fatalf("continue with lister-capable store should be ok, got %v", err)
	}

	// continue without resume, store lacks SessionLister: error.
	if err := ValidateOptions(&minimalOnlyStore{}, true, false, false); err == nil {
		t.Fatal("expected error for continue without list capability")
	}

	// continue WITH resume, minimal store: ok (resume wins, no list needed).
	if err := ValidateOptions(&minimalOnlyStore{}, true, true, false); err != nil {
		t.Fatalf("continue+resume with minimal store should be ok, got %v", err)
	}

	// plain store, no conflicts: ok.
	if err := ValidateOptions(store, false, false, false); err != nil {
		t.Fatalf("plain store should be ok, got %v", err)
	}
}

// minimalOnlyStore implements only Store (no optional capabilities).
type minimalOnlyStore struct{}

func (m *minimalOnlyStore) Append(_ context.Context, _ SessionKey, _ []Entry) error { return nil }
func (m *minimalOnlyStore) Load(_ context.Context, _ SessionKey) ([]Entry, error)   { return nil, nil }
