// Package sessionstore defines the SessionStore adapter interface and its
// supporting types, plus a reference in-memory implementation. A SessionStore
// mirrors Claude Code session transcripts to an external backend (Redis, S3,
// a database, ...) so a session created on one host can be resumed on another.
//
// The design mirrors the Python and TypeScript SDKs: Store carries the two
// required methods (Append, Load); optional capabilities are expressed as
// small companion interfaces (SessionLister, Deleter, SubkeyLister,
// SummaryLister) that call sites probe with a type assertion. This is more
// idiomatic Go than a single fat interface returning ErrNotImplemented
// sentinels, and lets an adapter implement only what it supports.
package sessionstore

import (
	"context"
	"encoding/json"
)

// Entry is a single line of a session transcript — an opaque, JSON-safe blob.
//
// It is represented as json.RawMessage so entries pass through the store
// verbatim: Load must return entries deeply equal to what Append received
// (byte-equal serialization is NOT required — a backend like Postgres jsonb
// may reorder object keys). Using raw bytes avoids the key-reordering and
// float-precision loss that a map[string]any round-trip would introduce, and
// keeps the batcher's uuid dedup a single shallow parse of one field.
type Entry = json.RawMessage

// SessionKey addresses one transcript record.
//
// ProjectKey is the stable, filesystem-safe encoding of the working directory
// (see session.ProjectKeyForDirectory). SessionID is the session UUID. Subpath
// is empty for the main transcript; when non-empty it addresses a subagent or
// sidecar record and follows the on-disk layout, e.g. "subagents/agent-<id>".
// Treat Subpath as an opaque key suffix.
type SessionKey struct {
	ProjectKey string
	SessionID  string
	Subpath    string
}

// ListEntry is returned by SessionLister.ListSessions.
type ListEntry struct {
	SessionID string
	// MTime is the storage write time in Unix epoch milliseconds.
	MTime int64
}

// SummaryEntry is the incrementally-maintained per-session summary sidecar
// returned by SummaryLister.ListSessionSummaries. Data is SDK-private and must
// be persisted verbatim by the adapter (never interpreted). MTime is the
// sidecar's storage write time and must share a clock with ListEntry.MTime for
// the same session.
type SummaryEntry struct {
	SessionID string
	MTime     int64
	Data      map[string]any
}

// Store is the required SessionStore contract. Every adapter must implement
// Append and Load.
type Store interface {
	// Append persists a batch of entries under key, after they have already
	// been written to the local disk transcript. Called once per mirrored
	// batch. Adapters should dedupe by each entry's "uuid" field when present,
	// since a retried batch may re-deliver entries from a prior partial write.
	Append(ctx context.Context, key SessionKey, entries []Entry) error

	// Load returns the entries previously appended under key, in append order,
	// or (nil, nil) when the key is unknown.
	Load(ctx context.Context, key SessionKey) ([]Entry, error)
}

// SessionLister is the optional capability of enumerating a project's main
// sessions. Required for continue-conversation resume and ListSessionsFromStore.
type SessionLister interface {
	ListSessions(ctx context.Context, projectKey string) ([]ListEntry, error)
}

// Deleter is the optional capability of deleting a record. Deleting a main key
// (empty Subpath) must cascade to every subkey of that session. When a store
// does not implement Deleter, deletion is a no-op (fine for append-only
// backends).
type Deleter interface {
	Delete(ctx context.Context, key SessionKey) error
}

// SubkeyLister is the optional capability of enumerating a session's subkeys
// (subagent/sidecar records). Used during resume to materialize subagent
// transcripts and by ListSubagentsFromStore. The key passed in never carries a
// Subpath.
type SubkeyLister interface {
	ListSubkeys(ctx context.Context, key SessionKey) ([]string, error)
}

// SummaryLister is the optional capability of returning incrementally
// maintained session summaries in one batch call — the fast path for
// ListSessionsFromStore that avoids one Load per session.
type SummaryLister interface {
	ListSessionSummaries(ctx context.Context, projectKey string) ([]SummaryEntry, error)
}

// FlushMode controls how eagerly the mirror batcher flushes to the store.
type FlushMode string

const (
	// FlushModeBatched (default) flushes on a result message or when the
	// pending buffer overflows its size thresholds.
	FlushModeBatched FlushMode = "batched"
	// FlushModeEager schedules a background flush after every enqueued frame.
	FlushModeEager FlushMode = "eager"
)

// Capability-probe helpers. Call sites use these to decide whether an optional
// method is available before invoking it.

// AsSessionLister returns the store as a SessionLister if it implements one.
func AsSessionLister(s Store) (SessionLister, bool) { l, ok := s.(SessionLister); return l, ok }

// AsDeleter returns the store as a Deleter if it implements one.
func AsDeleter(s Store) (Deleter, bool) { d, ok := s.(Deleter); return d, ok }

// AsSubkeyLister returns the store as a SubkeyLister if it implements one.
func AsSubkeyLister(s Store) (SubkeyLister, bool) { l, ok := s.(SubkeyLister); return l, ok }

// AsSummaryLister returns the store as a SummaryLister if it implements one.
func AsSummaryLister(s Store) (SummaryLister, bool) { l, ok := s.(SummaryLister); return l, ok }
