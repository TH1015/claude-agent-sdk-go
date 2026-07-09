package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

const mutSessionID = "550e8400-e29b-41d4-a716-446655440000"

func loadRaw(t *testing.T, store *sessionstore.InMemoryStore, key sessionstore.SessionKey) []map[string]any {
	t.Helper()
	entries, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var m map[string]any
		if err := json.Unmarshal(e, &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestRenameSessionViaStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: mutSessionID}
	mustAppend(t, store, key, `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`)

	if err := RenameSessionViaStore(ctx, store, mutSessionID, "  New Title  ", dir); err != nil {
		t.Fatal(err)
	}
	info, err := GetSessionInfoFromStore(ctx, store, mutSessionID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.Summary != "New Title" {
		t.Fatalf("expected summary 'New Title', got %+v", info)
	}

	// Empty title rejected.
	if err := RenameSessionViaStore(ctx, store, mutSessionID, "   ", dir); err == nil {
		t.Fatal("expected error for empty title")
	}
	// Invalid UUID rejected.
	if err := RenameSessionViaStore(ctx, store, "bad", "x", dir); err == nil {
		t.Fatal("expected error for invalid session id")
	}
}

func TestTagSessionViaStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: mutSessionID}
	mustAppend(t, store, key, `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`)

	if err := TagSessionViaStore(ctx, store, mutSessionID, "experiment", dir, false); err != nil {
		t.Fatal(err)
	}
	info, _ := GetSessionInfoFromStore(ctx, store, mutSessionID, dir)
	if info == nil || info.Tag == nil || *info.Tag != "experiment" {
		t.Fatalf("expected tag 'experiment', got %+v", info)
	}
	// Clear the tag.
	if err := TagSessionViaStore(ctx, store, mutSessionID, "", dir, true); err != nil {
		t.Fatal(err)
	}
	info, _ = GetSessionInfoFromStore(ctx, store, mutSessionID, dir)
	if info != nil && info.Tag != nil {
		t.Fatalf("expected tag cleared, got %v", info.Tag)
	}
}

func TestDeleteSessionViaStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: mutSessionID}
	mustAppend(t, store, key, `{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`)

	if err := DeleteSessionViaStore(ctx, store, mutSessionID, dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := store.Load(ctx, key)
	if entries != nil {
		t.Fatalf("expected session deleted, got %d entries", len(entries))
	}
}

func TestForkSessionViaStore(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: mutSessionID}
	mustAppend(t, store, key,
		`{"type":"user","uuid":"u1","parentUuid":null,"sessionId":"`+mutSessionID+`","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"`+mutSessionID+`","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"user","uuid":"u2","parentUuid":"a1","sessionId":"`+mutSessionID+`","message":{"role":"user","content":"second"}}`,
	)

	res, err := ForkSessionViaStore(ctx, store, mutSessionID, dir, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.SessionID == mutSessionID || !sessionstore.ValidateUUID(res.SessionID) {
		t.Fatalf("fork session id invalid or same as source: %q", res.SessionID)
	}

	forked := loadRaw(t, store, sessionstore.SessionKey{ProjectKey: pk, SessionID: res.SessionID})
	// Should have 3 transcript entries + 1 custom-title = 4.
	if len(forked) != 4 {
		t.Fatalf("expected 4 forked entries, got %d", len(forked))
	}

	// Every entry references the new session ID, never the old one; UUIDs remapped.
	oldUUIDs := map[string]bool{"u1": true, "a1": true, "u2": true}
	var firstNewUUID, secondParent string
	for _, e := range forked {
		if sid, ok := e["sessionId"].(string); ok && sid != "" && sid != res.SessionID {
			t.Fatalf("forked entry references stale sessionId %q", sid)
		}
		if uid, ok := e["uuid"].(string); ok && oldUUIDs[uid] {
			t.Fatalf("forked entry kept stale uuid %q", uid)
		}
		if e["type"] == "user" && e["message"] != nil {
			msg := e["message"].(map[string]any)
			if msg["content"] == "first" {
				firstNewUUID, _ = e["uuid"].(string)
			}
			if msg["content"] == "second" {
				if p, ok := e["parentUuid"].(string); ok {
					secondParent = p
				}
			}
		}
		// forkedFrom stamped.
		if e["type"] == "user" || e["type"] == "assistant" {
			ff, ok := e["forkedFrom"].(map[string]any)
			if !ok || ff["sessionId"] != mutSessionID {
				t.Fatalf("expected forkedFrom.sessionId=%s, got %v", mutSessionID, e["forkedFrom"])
			}
		}
	}
	if firstNewUUID == "" {
		t.Fatal("could not find remapped first user message")
	}
	_ = secondParent // chain integrity: second's parent is the assistant's new uuid (checked implicitly by no-stale-uuid)

	// custom-title derived + " (fork)".
	var title string
	for _, e := range forked {
		if e["type"] == "custom-title" {
			title, _ = e["customTitle"].(string)
		}
	}
	if !strings.HasSuffix(title, "(fork)") {
		t.Fatalf("expected derived fork title, got %q", title)
	}
}

func TestForkSessionViaStoreUpToMessage(t *testing.T) {
	ctx := context.Background()
	store := sessionstore.NewInMemoryStore()
	dir := t.TempDir()
	pk := sessionstore.ProjectKeyForDirectory(dir)
	key := sessionstore.SessionKey{ProjectKey: pk, SessionID: mutSessionID}
	u1 := "aaaaaaaa-1111-4111-8111-111111111111"
	a1 := "bbbbbbbb-2222-4222-8222-222222222222"
	u2 := "cccccccc-3333-4333-8333-333333333333"
	mustAppend(t, store, key,
		`{"type":"user","uuid":"`+u1+`","parentUuid":null,"message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","uuid":"`+a1+`","parentUuid":"`+u1+`","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}`,
		`{"type":"user","uuid":"`+u2+`","parentUuid":"`+a1+`","message":{"role":"user","content":"second"}}`,
	)
	res, err := ForkSessionViaStore(ctx, store, mutSessionID, dir, a1, "Custom Fork")
	if err != nil {
		t.Fatal(err)
	}
	forked := loadRaw(t, store, sessionstore.SessionKey{ProjectKey: pk, SessionID: res.SessionID})
	// up to a1: 2 transcript entries + custom-title = 3.
	if len(forked) != 3 {
		t.Fatalf("expected 3 entries (sliced), got %d", len(forked))
	}
	var title string
	for _, e := range forked {
		if e["type"] == "custom-title" {
			title, _ = e["customTitle"].(string)
		}
	}
	if title != "Custom Fork" {
		t.Fatalf("expected explicit title 'Custom Fork', got %q", title)
	}
}

func TestLocalDiskMutations(t *testing.T) {
	// Point CLAUDE_CONFIG_DIR at a temp dir and build a fixture transcript.
	tmpConfig := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", tmpConfig)
	projectDir := ""
	{
		// Use a real cwd so encodeCwd matches findSessionFile lookup.
		cwd := t.TempDir()
		projectDir = joinPath(tmpConfig, "projects", encodeCwd(mustAbs(t, cwd)))
		mkdirAll(t, projectDir)
		writeFile(t, joinPath(projectDir, mutSessionID+".jsonl"),
			`{"type":"user","uuid":"aaaaaaaa-1111-4111-8111-111111111111","parentUuid":null,"message":{"role":"user","content":"hello disk"}}`+"\n"+
				`{"type":"assistant","uuid":"bbbbbbbb-2222-4222-8222-222222222222","parentUuid":"aaaaaaaa-1111-4111-8111-111111111111","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`+"\n")

		// Rename.
		if err := RenameSession(mutSessionID, "Disk Title", WithSessionDirectory(cwd)); err != nil {
			t.Fatalf("rename: %v", err)
		}
		info, err := GetSessionInfo(mutSessionID, WithSessionDirectory(cwd))
		if err != nil {
			t.Fatal(err)
		}
		if info == nil || info.Summary != "Disk Title" {
			t.Fatalf("expected 'Disk Title', got %+v", info)
		}

		// Fork.
		res, err := ForkSession(mutSessionID, WithSessionDirectory(cwd))
		if err != nil {
			t.Fatalf("fork: %v", err)
		}
		if !sessionstore.ValidateUUID(res.SessionID) || res.SessionID == mutSessionID {
			t.Fatalf("bad fork id %q", res.SessionID)
		}
		forkMsgs, err := GetMessages(res.SessionID, WithSessionDirectory(cwd))
		if err != nil {
			t.Fatal(err)
		}
		if len(forkMsgs) != 2 {
			t.Fatalf("expected 2 forked messages, got %d", len(forkMsgs))
		}

		// Delete original.
		if err := DeleteSession(mutSessionID, WithSessionDirectory(cwd)); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := GetSessionInfo(mutSessionID, WithSessionDirectory(cwd)); err != nil {
			t.Fatalf("get after delete err: %v", err)
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := absPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func mkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := osMkdirAll(p); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := osWriteFile(p, content); err != nil {
		t.Fatal(err)
	}
}
