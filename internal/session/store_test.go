package session

import (
	"context"
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

const storeTestSessionID = "550e8400-e29b-41d4-a716-446655440000"

func mustAppend(t *testing.T, store *sessionstore.InMemoryStore, key sessionstore.SessionKey, jsons ...string) {
	t.Helper()
	entries := make([]sessionstore.Entry, len(jsons))
	for i, j := range jsons {
		entries[i] = sessionstore.Entry(j)
	}
	if err := store.Append(context.Background(), key, entries); err != nil {
		t.Fatal(err)
	}
}

func TestGetSessionMessagesFromStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: storeTestSessionID}

	mustAppend(t, store, key,
		`{"type":"user","uuid":"u1","parentUuid":null,"message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
	)

	msgs, err := GetSessionMessagesFromStore(ctx, store, storeTestSessionID, dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Type != "user" || msgs[1].Type != "assistant" {
		t.Fatalf("unexpected message types: %s, %s", msgs[0].Type, msgs[1].Type)
	}
}

func TestGetSessionInfoFromStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: storeTestSessionID}

	mustAppend(t, store, key,
		`{"type":"user","uuid":"u1","timestamp":"2024-01-01T00:00:00.000Z","message":{"role":"user","content":"first prompt here"}}`,
		`{"type":"custom-title","customTitle":"My Session","sessionId":"`+storeTestSessionID+`"}`,
	)

	info, err := GetSessionInfoFromStore(ctx, store, storeTestSessionID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil {
		t.Fatal("expected info, got nil")
	}
	if info.Summary != "My Session" {
		t.Fatalf("summary = %q, want My Session", info.Summary)
	}
	if info.CustomTitle == nil || *info.CustomTitle != "My Session" {
		t.Fatalf("custom title = %v", info.CustomTitle)
	}

	// Unknown session → nil.
	none, err := GetSessionInfoFromStore(ctx, store, "11111111-1111-1111-1111-111111111111", dir)
	if err != nil || none != nil {
		t.Fatalf("expected nil for unknown session, got (%v,%v)", none, err)
	}
}

func TestListSessionsFromStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)

	s1 := "11111111-1111-1111-1111-111111111111"
	s2 := "22222222-2222-2222-2222-222222222222"
	mustAppend(t, store, sessionstore.SessionKey{ProjectKey: pk, SessionID: s1},
		`{"type":"user","uuid":"u1","timestamp":"2024-01-01T00:00:00.000Z","message":{"role":"user","content":"prompt one"}}`)
	mustAppend(t, store, sessionstore.SessionKey{ProjectKey: pk, SessionID: s2},
		`{"type":"user","uuid":"u2","timestamp":"2024-01-01T00:00:01.000Z","message":{"role":"user","content":"prompt two"}}`)

	infos, err := ListSessionsFromStore(ctx, store, dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(infos))
	}
	// Sorted by last_modified desc: s2 (newer) first.
	if infos[0].SessionID != s2 {
		t.Fatalf("expected newest session first (%s), got %s", s2, infos[0].SessionID)
	}
}

func TestSubagentsFromStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)

	mustAppend(t, store, sessionstore.SessionKey{ProjectKey: pk, SessionID: storeTestSessionID},
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"main"}}`)
	subKey := sessionstore.SessionKey{ProjectKey: pk, SessionID: storeTestSessionID, Subpath: "subagents/agent-abc"}
	mustAppend(t, store, subKey,
		`{"type":"agent_metadata","name":"researcher"}`,
		`{"type":"user","uuid":"su1","parentUuid":null,"message":{"role":"user","content":"sub task"}}`,
		`{"type":"assistant","uuid":"sa1","parentUuid":"su1","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	)

	ids, err := ListSubagentsFromStore(ctx, store, storeTestSessionID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("expected [abc], got %v", ids)
	}

	msgs, err := GetSubagentMessagesFromStore(ctx, store, storeTestSessionID, "abc", dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 subagent messages, got %d", len(msgs))
	}
	if msgs[0].Type != "user" || msgs[1].Type != "assistant" {
		t.Fatalf("unexpected subagent message types: %s, %s", msgs[0].Type, msgs[1].Type)
	}
}

func TestListSubagentsFromStoreRequiresSubkeyLister(t *testing.T) {
	ctx := context.Background()
	// minimalStore implements only Store (no SubkeyLister).
	store := &minimalStore{}
	_, err := ListSubagentsFromStore(ctx, store, storeTestSessionID, ".")
	if err == nil {
		t.Fatal("expected StoreCapabilityError, got nil")
	}
}

// minimalStore implements only the required Store methods.
type minimalStore struct{}

func (m *minimalStore) Append(context.Context, sessionstore.SessionKey, []sessionstore.Entry) error {
	return nil
}
func (m *minimalStore) Load(context.Context, sessionstore.SessionKey) ([]sessionstore.Entry, error) {
	return nil, nil
}
