package subprocess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
	"github.com/TH1015/claude-agent-sdk-go/internal/shared"
)

// newMirrorMockCLI writes a mock CLI that emits one transcript_mirror frame
// (for a main transcript under projectsDir) followed by a result message.
func newMirrorMockCLI(t *testing.T, projectsDir, projectKey, sessionID string) string {
	t.Helper()
	filePath := filepath.Join(projectsDir, projectKey, sessionID+".jsonl")
	mirror := map[string]any{
		"type":     "transcript_mirror",
		"filePath": filePath,
		"entries": []any{
			map[string]any{"type": "user", "uuid": "u1", "n": 1},
			map[string]any{"type": "assistant", "uuid": "a1", "n": 2},
		},
	}
	mirrorJSON, _ := json.Marshal(mirror)
	result := `{"type":"result","subtype":"success","duration_ms":1,"duration_api_ms":1,"is_error":false,"num_turns":1,"session_id":"` + sessionID + `"}`

	var script, ext string
	if runtime.GOOS == windowsOS {
		ext = ".bat"
		script = "@echo off\r\n" +
			"if \"%1\"==\"-v\" (echo 3.0.0 & exit /b 0)\r\n" +
			"echo " + string(mirrorJSON) + "\r\n" +
			"echo " + result + "\r\n"
	} else {
		ext = ""
		script = "#!/bin/bash\n" +
			"if [ \"$1\" = \"-v\" ]; then echo \"3.0.0\"; exit 0; fi\n" +
			"cat <<'EOF'\n" + string(mirrorJSON) + "\n" + result + "\nEOF\n" +
			"sleep 0.3\n"
	}
	return createTransportTempScript(script, ext)
}

// TestMirrorInterception verifies transcript_mirror frames are forwarded to the
// SessionStore and never delivered to the consumer, and that the store holds
// the entries after the result flush + close.
func TestMirrorInterception(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tmpConfig := t.TempDir()
	projectsDir := filepath.Join(tmpConfig, "projects")
	projectKey := "-proj-key"
	sessionID := "550e8400-e29b-41d4-a716-446655440000"

	store := sessionstore.NewInMemoryStore()
	cliPath := newMirrorMockCLI(t, projectsDir, projectKey, sessionID)
	defer os.Remove(cliPath)

	options := &shared.Options{
		SessionStore: store,
		ExtraEnv:     map[string]string{"CLAUDE_CONFIG_DIR": tmpConfig},
	}
	transport := New(cliPath, options, false, "sdk-go")
	if err := transport.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = transport.Close() }()

	msgChan, errChan := transport.ReceiveMessages(ctx)

	var gotResult bool
	var sawMirror bool
	timeout := time.After(5 * time.Second)
loop:
	for {
		select {
		case msg, ok := <-msgChan:
			if !ok {
				break loop
			}
			switch msg.(type) {
			case *shared.TranscriptMirrorMessage:
				sawMirror = true
			case *shared.ResultMessage:
				gotResult = true
			}
		case err := <-errChan:
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}
		case <-timeout:
			break loop
		}
		if gotResult {
			break
		}
	}

	if sawMirror {
		t.Error("transcript_mirror frame leaked to consumer msgChan")
	}
	if !gotResult {
		t.Fatal("did not receive result message")
	}

	// Close flushes any remaining batch.
	_ = transport.Close()

	key := sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID}
	entries, err := store.Load(ctx, key)
	if err != nil {
		t.Fatalf("store load: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 mirrored entries in store, got %d", len(entries))
	}
}
