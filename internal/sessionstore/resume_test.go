package sessionstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testSessionID = "550e8400-e29b-41d4-a716-446655440000"

func TestMaterializeResumeExplicit(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	cwd := t.TempDir()
	pk := ProjectKeyForDirectory(cwd)
	key := SessionKey{ProjectKey: pk, SessionID: testSessionID}
	mustNil(t, store.Append(ctx, key, []Entry{
		Entry(`{"type":"user","uuid":"u1","sessionId":"` + testSessionID + `"}`),
		Entry(`{"type":"assistant","uuid":"a1","sessionId":"` + testSessionID + `"}`),
	}))
	// Add a subagent transcript.
	subKey := key
	subKey.Subpath = "subagents/agent-x"
	mustNil(t, store.Append(ctx, subKey, []Entry{
		Entry(`{"type":"user","uuid":"su1"}`),
		Entry(`{"type":"agent_metadata","name":"x"}`),
	}))

	mat, err := MaterializeResumeSession(ctx, store, testSessionID, false, cwd, nil, 0)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if mat == nil {
		t.Fatal("expected materialization, got nil")
	}
	defer mat.Cleanup()

	if mat.ResumeSessionID != testSessionID {
		t.Fatalf("resume id = %q, want %q", mat.ResumeSessionID, testSessionID)
	}

	// Main transcript file exists with 2 lines.
	mainFile := filepath.Join(mat.ProjectsDir, pk, testSessionID+".jsonl")
	data, err := os.ReadFile(mainFile)
	if err != nil {
		t.Fatalf("read main transcript: %v", err)
	}
	if got := countLines(data); got != 2 {
		t.Fatalf("main transcript lines = %d, want 2", got)
	}

	// Subagent transcript file exists (1 transcript line; metadata split out).
	subFile := filepath.Join(mat.ProjectsDir, pk, testSessionID, "subagents", "agent-x.jsonl")
	subData, err := os.ReadFile(subFile)
	if err != nil {
		t.Fatalf("read subagent transcript: %v", err)
	}
	if got := countLines(subData); got != 1 {
		t.Fatalf("subagent transcript lines = %d, want 1", got)
	}
	// Metadata sidecar exists.
	metaFile := filepath.Join(mat.ProjectsDir, pk, testSessionID, "subagents", "agent-x.meta.json")
	if _, err := os.Stat(metaFile); err != nil {
		t.Fatalf("expected metadata sidecar: %v", err)
	}
}

func TestMaterializeResumeNoStoreOrNoResume(t *testing.T) {
	ctx := context.Background()
	// No store.
	mat, err := MaterializeResumeSession(ctx, nil, testSessionID, false, ".", nil, 0)
	if err != nil || mat != nil {
		t.Fatalf("nil store should return (nil,nil), got (%v,%v)", mat, err)
	}
	// Store but no resume/continue.
	store := NewInMemoryStore()
	mat, err = MaterializeResumeSession(ctx, store, "", false, ".", nil, 0)
	if err != nil || mat != nil {
		t.Fatalf("no resume should return (nil,nil), got (%v,%v)", mat, err)
	}
	// Resume unknown session → (nil,nil).
	mat, err = MaterializeResumeSession(ctx, store, testSessionID, false, ".", nil, 0)
	if err != nil || mat != nil {
		t.Fatalf("unknown session should return (nil,nil), got (%v,%v)", mat, err)
	}
	// Non-UUID resume → (nil,nil).
	mat, err = MaterializeResumeSession(ctx, store, "not-a-uuid", false, ".", nil, 0)
	if err != nil || mat != nil {
		t.Fatalf("non-uuid resume should return (nil,nil), got (%v,%v)", mat, err)
	}
}

func TestMaterializeResumeContinue(t *testing.T) {
	ctx := context.Background()
	store := NewInMemoryStore()
	cwd := t.TempDir()
	pk := ProjectKeyForDirectory(cwd)
	older := "11111111-1111-1111-1111-111111111111"
	newer := "22222222-2222-2222-2222-222222222222"
	mustNil(t, store.Append(ctx, SessionKey{ProjectKey: pk, SessionID: older}, []Entry{Entry(`{"type":"user","uuid":"o1"}`)}))
	mustNil(t, store.Append(ctx, SessionKey{ProjectKey: pk, SessionID: newer}, []Entry{Entry(`{"type":"user","uuid":"n1"}`)}))

	mat, err := MaterializeResumeSession(ctx, store, "", true, cwd, nil, 0)
	if err != nil {
		t.Fatalf("materialize continue: %v", err)
	}
	if mat == nil {
		t.Fatal("expected materialization for continue")
	}
	defer mat.Cleanup()
	// Newer session (higher mtime) should be resolved.
	if mat.ResumeSessionID != newer {
		t.Fatalf("continue resolved %q, want newer %q", mat.ResumeSessionID, newer)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
