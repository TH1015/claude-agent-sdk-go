//go:build ignore

// Command main is a runnable demo of the Redis SessionStore adapter: it runs a
// query with the store attached, captures the session ID, then resumes from the
// store in a second query. Requires a running Redis at localhost:6379 and the
// Claude CLI on PATH.
//
//	go run -tags ignore ./examples/session-stores/redis
//
// (Or copy store.go into your project and adapt this to your needs.)
package main

import (
	"context"
	"fmt"
	"log"

	claudecode "github.com/TH1015/claude-agent-sdk-go"
	redisstore "github.com/TH1015/claude-agent-sdk-go/examples/session-stores/redis"

	"github.com/redis/go-redis/v9"
)

func main() {
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer func() { _ = client.Close() }()

	store := redisstore.New(client, "claude-sessions")

	// First query: mirror the transcript to Redis, capture the session ID.
	var sessionID string
	iter, err := claudecode.Query(ctx, "List the Go files in this repo.",
		claudecode.WithSessionStore(store),
	)
	if err != nil {
		log.Fatal(err)
	}
	for {
		msg, err := iter.Next(ctx)
		if err != nil {
			break
		}
		if r, ok := msg.(*claudecode.ResultMessage); ok {
			sessionID = r.SessionID
		}
	}
	_ = iter.Close()
	fmt.Println("session:", sessionID)

	// Second query (possibly on another host): resume from the store.
	iter2, err := claudecode.Query(ctx, "Summarize what those files do.",
		claudecode.WithSessionStore(store),
		claudecode.WithResume(sessionID),
	)
	if err != nil {
		log.Fatal(err)
	}
	for {
		msg, err := iter2.Next(ctx)
		if err != nil {
			break
		}
		if r, ok := msg.(*claudecode.ResultMessage); ok && r.Result != nil {
			fmt.Println(*r.Result)
		}
	}
	_ = iter2.Close()
}
