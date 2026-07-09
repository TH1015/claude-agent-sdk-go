# Redis SessionStore adapter

A runnable reference `SessionStore` adapter backed by Redis, mirroring the
design of the TypeScript SDK's `RedisSessionStore`. Copy `store.go` into your
own project and construct it with a configured `*redis.Client` — it is a
reference, not a published package.

## Storage model

| Data | Redis structure | Key |
| :--- | :--- | :--- |
| Record entries | list (`RPUSH` / `LRANGE`) | `<prefix>:e:{<project>/<session>}[:<subpath>]` |
| Project session index | sorted set (score = mtime ms) | `<prefix>:idx:<project>` |
| Session subkeys | set | `<prefix>:sub:{<project>/<session>}` |
| Session summaries | hash (field = session id) | `<prefix>:summ:<project>` |

It implements every optional capability: `SessionLister`, `SessionDeleter`,
`SessionSubkeyLister`, and `SessionSummaryLister` (the summary fast path for
`ListSessionsFromStore`).

## Usage

```go
import (
    claudecode "github.com/TH1015/claude-agent-sdk-go"
    redisstore "github.com/TH1015/claude-agent-sdk-go/examples/session-stores/redis"
    "github.com/redis/go-redis/v9"
)

client := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
store := redisstore.New(client, "claude-sessions")

// Mirror this session to Redis.
iter, _ := claudecode.Query(ctx, "Hello!", claudecode.WithSessionStore(store))
// ... capture the session id from the ResultMessage ...

// Later, possibly on another host, resume from Redis.
iter2, _ := claudecode.Query(ctx, "Continue where we left off",
    claudecode.WithSessionStore(store),
    claudecode.WithResume(sessionID),
)
```

See `main.go` (build tag `ignore`) for a complete runnable demo.

## Idempotency

Mirror writes are best-effort: a failed `Append` is retried, so a batch may be
re-delivered. Production adapters should dedupe by each entry's `uuid`. This
reference keeps `RPUSH` simple; to dedupe, maintain a per-record `SET` of seen
UUIDs and skip entries already present before pushing.

## Running the conformance suite

`store_test.go` runs the SDK's shared conformance suite against an in-process
`miniredis`, so no external server is required:

```
go test ./examples/session-stores/redis/
```
