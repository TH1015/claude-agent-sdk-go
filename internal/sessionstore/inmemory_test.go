package sessionstore_test

import (
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
	"github.com/TH1015/claude-agent-sdk-go/sessionstore/conformance"
)

// TestInMemoryStoreConformance runs the full SessionStore conformance suite
// against the reference InMemoryStore, which implements every optional
// capability.
func TestInMemoryStoreConformance(t *testing.T) {
	conformance.RunConformance(t, func() sessionstore.Store {
		return sessionstore.NewInMemoryStore()
	})
}
