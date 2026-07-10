package redisstore

import (
	"context"
	"testing"

	claudecode "github.com/TH1015/claude-agent-sdk-go"
	"github.com/TH1015/claude-agent-sdk-go/sessionstore/conformance"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// TestRedisStoreConformance runs the full SessionStore conformance suite against
// the Redis adapter, backed by an in-process miniredis so no external server is
// required. Each contract gets a fresh Redis (via FlushAll) for isolation.
func TestRedisStoreConformance(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()

	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	conformance.RunConformance(t, func() claudecode.SessionStore {
		mr.FlushAll()
		return New(client, "test")
	})
}

// TestRedisStoreDedupesByUUID verifies a re-delivered batch (as the mirror
// batcher's retry produces) does not duplicate transcript entries: entries with
// a "uuid" are deduped, entries without one are always appended.
func TestRedisStoreDedupesByUUID(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	store := New(client, "dedup")
	key := claudecode.SessionKey{ProjectKey: "p", SessionID: "s"}

	batch := []claudecode.SessionStoreEntry{
		claudecode.SessionStoreEntry(`{"type":"user","uuid":"u1"}`),
		claudecode.SessionStoreEntry(`{"type":"assistant","uuid":"a1"}`),
	}
	if err := store.Append(ctx, key, batch); err != nil {
		t.Fatal(err)
	}
	// Re-deliver the exact same batch (simulating a retry after a partial write).
	if err := store.Append(ctx, key, batch); err != nil {
		t.Fatal(err)
	}
	// Plus a partially-overlapping batch: u1 again + a new u2.
	if err := store.Append(ctx, key, []claudecode.SessionStoreEntry{
		claudecode.SessionStoreEntry(`{"type":"user","uuid":"u1"}`),
		claudecode.SessionStoreEntry(`{"type":"user","uuid":"u2"}`),
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("expected 3 deduped entries (u1,a1,u2), got %d: %v", len(loaded), loaded)
	}

	// Entries WITHOUT a uuid are always appended (not deduped).
	noUUID := []claudecode.SessionStoreEntry{claudecode.SessionStoreEntry(`{"type":"tag","tag":"x"}`)}
	if err := store.Append(ctx, key, noUUID); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(ctx, key, noUUID); err != nil {
		t.Fatal(err)
	}
	loaded, _ = store.Load(ctx, key)
	if len(loaded) != 5 {
		t.Fatalf("expected 5 entries after two no-uuid appends, got %d", len(loaded))
	}
}
