package claudecode

import (
	"github.com/TH1015/claude-agent-sdk-go/internal/session"
	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// SessionStore mirrors Claude Code session transcripts to an external backend
// (Redis, S3, a database, ...) so a session created on one host can be resumed
// on another. Implement Append and Load at minimum; add SessionLister,
// Deleter, SubkeyLister, and/or SummaryLister to unlock the corresponding
// higher-level operations.
type SessionStore = sessionstore.Store

// SessionKey addresses one transcript record. Subpath is empty for the main
// transcript and set (e.g. "subagents/agent-<id>") for subagent/sidecar records.
type SessionKey = sessionstore.SessionKey

// SessionStoreEntry is one line of a session transcript — an opaque, JSON-safe
// blob (json.RawMessage). Load must return entries deeply equal to what Append
// received.
type SessionStoreEntry = sessionstore.Entry

// SessionStoreListEntry is returned by a SessionStore's ListSessions.
type SessionStoreListEntry = sessionstore.ListEntry

// SessionSummaryEntry is the per-session summary sidecar returned by a
// SessionStore's ListSessionSummaries.
type SessionSummaryEntry = sessionstore.SummaryEntry

// Optional SessionStore capability interfaces. A store need only implement the
// ones it supports; call sites probe with a type assertion.
type (
	// SessionLister enumerates a project's main sessions.
	SessionLister = sessionstore.SessionLister
	// SessionDeleter deletes a record (main-key deletes cascade to subkeys).
	SessionDeleter = sessionstore.Deleter
	// SessionSubkeyLister enumerates a session's subagent/sidecar subkeys.
	SessionSubkeyLister = sessionstore.SubkeyLister
	// SessionSummaryLister returns incrementally maintained session summaries.
	SessionSummaryLister = sessionstore.SummaryLister
)

// SessionStoreFlushMode controls how eagerly the mirror batcher flushes.
type SessionStoreFlushMode = sessionstore.FlushMode

// Session store flush modes.
const (
	// SessionStoreFlushBatched (default) flushes on a result message or size
	// threshold overflow.
	SessionStoreFlushBatched = sessionstore.FlushModeBatched
	// SessionStoreFlushEager schedules a background flush after every frame.
	SessionStoreFlushEager = sessionstore.FlushModeEager
)

// NewInMemorySessionStore returns an in-memory SessionStore for development and
// testing. Data is lost when the process exits.
func NewInMemorySessionStore() *sessionstore.InMemoryStore {
	return sessionstore.NewInMemoryStore()
}

// ProjectKeyForDirectory derives the SessionStore project_key for a directory,
// using the same canonicalization the CLI applies to project directory names.
// When dir is empty, the current working directory is used.
func ProjectKeyForDirectory(dir string) string {
	return session.ProjectKeyForDirectory(dir)
}

// FoldSessionSummary folds a batch of entries into a running session summary.
// SessionStore adapters call this from inside Append to maintain a
// SessionSummaryEntry sidecar incrementally. Do not call it for keys with a
// non-empty Subpath. prev is nil on the first append.
func FoldSessionSummary(prev *SessionSummaryEntry, key SessionKey, entries []SessionStoreEntry) SessionSummaryEntry {
	return sessionstore.FoldSessionSummary(prev, key, entries)
}
