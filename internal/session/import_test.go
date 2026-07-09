package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

func TestImportSessionToStore(t *testing.T) {
	ctx := context.Background()
	tmpConfig := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmpConfig)
	cwd := t.TempDir()

	abs, err := filepath.Abs(cwd)
	if err != nil {
		t.Fatal(err)
	}
	projectDirName := encodeCwd(abs)
	projectDir := filepath.Join(tmpConfig, "projects", projectDirName)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "550e8400-e29b-41d4-a716-446655440000"
	transcript := `{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hello"}}` + "\n" +
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, sid+".jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	// A subagent transcript + meta sidecar.
	subDir := filepath.Join(projectDir, sid, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-x.jsonl"),
		[]byte(`{"type":"user","uuid":"su1","message":{"role":"user","content":"sub"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "agent-x.meta.json"),
		[]byte(`{"agentType":"researcher"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	store := sessionstore.NewInMemoryStore()
	if err := ImportSessionToStore(ctx, sid, store, cwd, true, 0); err != nil {
		t.Fatalf("import: %v", err)
	}

	// Main transcript readable from store.
	key := sessionstore.SessionKey{ProjectKey: projectDirName, SessionID: sid}
	entries, err := store.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 main entries in store, got %d", len(entries))
	}

	// Subagent subkey present, including a synthetic agent_metadata entry.
	subkeys, err := store.ListSubkeys(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(subkeys) != 1 || subkeys[0] != "subagents/agent-x" {
		t.Fatalf("expected [subagents/agent-x], got %v", subkeys)
	}
	subEntries, _ := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectDirName, SessionID: sid, Subpath: "subagents/agent-x"})
	// 1 transcript line + 1 agent_metadata sidecar = 2.
	if len(subEntries) != 2 {
		t.Fatalf("expected 2 subagent entries (transcript + metadata), got %d", len(subEntries))
	}

	// Round-trip: read messages back via the store variant.
	msgs, err := GetSessionMessagesFromStore(ctx, store, sid, cwd, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 round-trip messages, got %d", len(msgs))
	}
}
