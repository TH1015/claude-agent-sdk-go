package cli

import (
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
	"github.com/TH1015/claude-agent-sdk-go/internal/shared"
)

// TestSessionMirrorFlag verifies --session-mirror is added exactly when a
// SessionStore is configured, for both streaming and one-shot command builds.
// Without this flag the real CLI never emits transcript_mirror frames, so the
// store stays empty (Python SDK parity: --session-mirror when session_store set).
func TestSessionMirrorFlag(t *testing.T) {
	store := sessionstore.NewInMemoryStore()

	t.Run("present when store set (BuildCommand)", func(t *testing.T) {
		cmd := BuildCommand("/usr/local/bin/claude", &shared.Options{SessionStore: store}, false)
		assertContainsArg(t, cmd, "--session-mirror")
	})
	t.Run("present when store set (BuildCommandWithPrompt)", func(t *testing.T) {
		cmd := BuildCommandWithPrompt("/usr/local/bin/claude", &shared.Options{SessionStore: store}, "hi")
		assertContainsArg(t, cmd, "--session-mirror")
	})
	t.Run("absent when no store", func(t *testing.T) {
		cmd := BuildCommand("/usr/local/bin/claude", &shared.Options{}, false)
		assertNotContainsArg(t, cmd, "--session-mirror")
	})
}
