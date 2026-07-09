package redisstore

import (
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
