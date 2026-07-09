// Package redisstore provides a reference SessionStore adapter backed by Redis.
//
// It mirrors the design of the TypeScript SDK's RedisSessionStore:
//   - each record's entries live in a Redis list (RPUSH / LRANGE), keyed by
//     project/session[/subpath];
//   - a per-project sorted set (ZADD, score = storage mtime in epoch ms) indexes
//     the project's main sessions for ListSessions;
//   - a per-project hash holds the incrementally-folded session summaries for
//     the SummaryLister fast path.
//
// Copy this file into your project and construct it with a configured
// *redis.Client. It is a runnable reference, not a published package.
package redisstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	claudecode "github.com/TH1015/claude-agent-sdk-go"
	"github.com/redis/go-redis/v9"
)

// Store is a Redis-backed SessionStore. It implements the required Append/Load
// plus every optional capability (SessionLister, Deleter, SubkeyLister,
// SummaryLister).
type Store struct {
	client *redis.Client
	prefix string
}

// Compile-time assertions that Store satisfies every capability.
var (
	_ claudecode.SessionStore         = (*Store)(nil)
	_ claudecode.SessionLister        = (*Store)(nil)
	_ claudecode.SessionDeleter       = (*Store)(nil)
	_ claudecode.SessionSubkeyLister  = (*Store)(nil)
	_ claudecode.SessionSummaryLister = (*Store)(nil)
)

// New creates a Redis-backed session store. prefix namespaces all keys (e.g.
// "claude:sessions"); an empty prefix defaults to "claude-sessions".
func New(client *redis.Client, prefix string) *Store {
	if prefix == "" {
		prefix = "claude-sessions"
	}
	return &Store{client: client, prefix: prefix}
}

// entriesKey holds the JSONL entries list for a record.
func (s *Store) entriesKey(key claudecode.SessionKey) string {
	if key.Subpath != "" {
		return fmt.Sprintf("%s:e:{%s/%s}:%s", s.prefix, key.ProjectKey, key.SessionID, key.Subpath)
	}
	return fmt.Sprintf("%s:e:{%s/%s}", s.prefix, key.ProjectKey, key.SessionID)
}

// sessionsIndexKey is the sorted set of main session IDs for a project.
func (s *Store) sessionsIndexKey(projectKey string) string {
	return fmt.Sprintf("%s:idx:%s", s.prefix, projectKey)
}

// subkeysIndexKey is the set of subpaths for a session.
func (s *Store) subkeysIndexKey(projectKey, sessionID string) string {
	return fmt.Sprintf("%s:sub:{%s/%s}", s.prefix, projectKey, sessionID)
}

// summariesKey is the hash of session summaries for a project (field = sessionID).
func (s *Store) summariesKey(projectKey string) string {
	return fmt.Sprintf("%s:summ:%s", s.prefix, projectKey)
}

// Append persists a batch of entries and maintains the index + summary sidecar.
//
// Idempotency note: because a retried mirror batch may re-deliver entries from
// a prior partial write, a production adapter should dedupe by entry "uuid".
// This reference keeps the RPUSH simple; see the README for a dedup sketch.
func (s *Store) Append(ctx context.Context, key claudecode.SessionKey, entries []claudecode.SessionStoreEntry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := s.client.TxPipeline()

	vals := make([]any, len(entries))
	for i, e := range entries {
		vals[i] = string(e)
	}
	pipe.RPush(ctx, s.entriesKey(key), vals...)

	nowMS := time.Now().UnixMilli()
	if key.Subpath == "" {
		pipe.ZAdd(ctx, s.sessionsIndexKey(key.ProjectKey), redis.Z{Score: float64(nowMS), Member: key.SessionID})
	} else {
		pipe.SAdd(ctx, s.subkeysIndexKey(key.ProjectKey, key.SessionID), key.Subpath)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	if key.Subpath == "" {
		if err := s.foldSummary(ctx, key, entries, nowMS); err != nil {
			return err
		}
	}
	return nil
}

// foldSummary reads the prior summary, folds the batch, stamps mtime, and
// writes it back.
func (s *Store) foldSummary(ctx context.Context, key claudecode.SessionKey, entries []claudecode.SessionStoreEntry, nowMS int64) error {
	summKey := s.summariesKey(key.ProjectKey)
	var prev *claudecode.SessionSummaryEntry
	if raw, err := s.client.HGet(ctx, summKey, key.SessionID).Result(); err == nil {
		var se claudecode.SessionSummaryEntry
		if json.Unmarshal([]byte(raw), &se) == nil {
			prev = &se
		}
	} else if err != redis.Nil {
		return err
	}
	folded := claudecode.FoldSessionSummary(prev, key, entries)
	folded.MTime = nowMS
	b, err := json.Marshal(folded)
	if err != nil {
		return err
	}
	return s.client.HSet(ctx, summKey, key.SessionID, string(b)).Err()
}

// Load returns a copy of the entries stored under key, or (nil, nil) when
// unknown.
func (s *Store) Load(ctx context.Context, key claudecode.SessionKey) ([]claudecode.SessionStoreEntry, error) {
	vals, err := s.client.LRange(ctx, s.entriesKey(key), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) == 0 {
		return nil, nil
	}
	out := make([]claudecode.SessionStoreEntry, len(vals))
	for i, v := range vals {
		out[i] = claudecode.SessionStoreEntry(v)
	}
	return out, nil
}

// ListSessions returns main-session IDs for a project with their mtimes.
func (s *Store) ListSessions(ctx context.Context, projectKey string) ([]claudecode.SessionStoreListEntry, error) {
	zs, err := s.client.ZRangeWithScores(ctx, s.sessionsIndexKey(projectKey), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]claudecode.SessionStoreListEntry, 0, len(zs))
	for _, z := range zs {
		sid, _ := z.Member.(string)
		out = append(out, claudecode.SessionStoreListEntry{SessionID: sid, MTime: int64(z.Score)})
	}
	return out, nil
}

// ListSessionSummaries returns the folded summary sidecars for a project.
func (s *Store) ListSessionSummaries(ctx context.Context, projectKey string) ([]claudecode.SessionSummaryEntry, error) {
	m, err := s.client.HGetAll(ctx, s.summariesKey(projectKey)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]claudecode.SessionSummaryEntry, 0, len(m))
	for _, raw := range m {
		var se claudecode.SessionSummaryEntry
		if json.Unmarshal([]byte(raw), &se) == nil {
			out = append(out, se)
		}
	}
	return out, nil
}

// Delete removes a record. Deleting a main key cascades to all subkeys.
func (s *Store) Delete(ctx context.Context, key claudecode.SessionKey) error {
	if key.Subpath != "" {
		pipe := s.client.TxPipeline()
		pipe.Del(ctx, s.entriesKey(key))
		pipe.SRem(ctx, s.subkeysIndexKey(key.ProjectKey, key.SessionID), key.Subpath)
		_, err := pipe.Exec(ctx)
		return err
	}

	subpaths, err := s.client.SMembers(ctx, s.subkeysIndexKey(key.ProjectKey, key.SessionID)).Result()
	if err != nil && err != redis.Nil {
		return err
	}
	pipe := s.client.TxPipeline()
	pipe.Del(ctx, s.entriesKey(key))
	pipe.ZRem(ctx, s.sessionsIndexKey(key.ProjectKey), key.SessionID)
	pipe.HDel(ctx, s.summariesKey(key.ProjectKey), key.SessionID)
	for _, sp := range subpaths {
		pipe.Del(ctx, s.entriesKey(claudecode.SessionKey{ProjectKey: key.ProjectKey, SessionID: key.SessionID, Subpath: sp}))
	}
	pipe.Del(ctx, s.subkeysIndexKey(key.ProjectKey, key.SessionID))
	_, err = pipe.Exec(ctx)
	return err
}

// ListSubkeys returns the subpaths stored under a session.
func (s *Store) ListSubkeys(ctx context.Context, key claudecode.SessionKey) ([]string, error) {
	members, err := s.client.SMembers(ctx, s.subkeysIndexKey(key.ProjectKey, key.SessionID)).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	return members, nil
}
