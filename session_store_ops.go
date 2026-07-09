package claudecode

import (
	"context"

	"github.com/TH1015/claude-agent-sdk-go/internal/session"
)

// ListSubagents lists subagent IDs for a session by scanning its subagents
// directory on local disk. Returns an empty slice if the session is not found
// or session_id is not a valid UUID.
func ListSubagents(sessionID string, opts ...SessionOption) []string {
	return session.ListSubagents(sessionID, opts...)
}

// GetSubagentMessages reads a subagent's conversation messages from its local
// transcript. Returns an empty slice if the session/subagent is not found.
func GetSubagentMessages(sessionID, agentID string, opts ...SessionOption) []SessionMessage {
	return session.GetSubagentMessages(sessionID, agentID, opts...)
}

// ListSessionsFromStore lists sessions from a SessionStore, sorted by
// last-modified descending. directory computes the project key (defaults to the
// current working directory when empty). Requires the store to implement
// SessionLister or SessionSummaryLister. Pass limit<=0 / offset<=0 to disable.
func ListSessionsFromStore(ctx context.Context, store SessionStore, directory string, limit, offset int) ([]SDKSessionInfo, error) {
	return session.ListSessionsFromStore(ctx, store, directory, limit, offset)
}

// GetSessionInfoFromStore reads metadata for a single session from a
// SessionStore. Returns nil if not found, the session_id is invalid, the
// session is a sidechain, or it has no extractable summary.
func GetSessionInfoFromStore(ctx context.Context, store SessionStore, sessionID, directory string) (*SDKSessionInfo, error) {
	return session.GetSessionInfoFromStore(ctx, store, sessionID, directory)
}

// GetSessionMessagesFromStore reads a session's conversation messages from a
// SessionStore, in chronological order.
func GetSessionMessagesFromStore(ctx context.Context, store SessionStore, sessionID, directory string, limit, offset int) ([]SessionMessage, error) {
	return session.GetSessionMessagesFromStore(ctx, store, sessionID, directory, limit, offset)
}

// ListSubagentsFromStore lists subagent IDs for a session from a SessionStore.
// Requires the store to implement SessionSubkeyLister.
func ListSubagentsFromStore(ctx context.Context, store SessionStore, sessionID, directory string) ([]string, error) {
	return session.ListSubagentsFromStore(ctx, store, sessionID, directory)
}

// GetSubagentMessagesFromStore reads a subagent's conversation messages from a
// SessionStore.
func GetSubagentMessagesFromStore(ctx context.Context, store SessionStore, sessionID, agentID, directory string, limit, offset int) ([]SessionMessage, error) {
	return session.GetSubagentMessagesFromStore(ctx, store, sessionID, agentID, directory, limit, offset)
}

// ForkSessionResult is the result of a fork operation.
type ForkSessionResult = session.ForkSessionResult

// RenameSession renames a local session by appending a custom-title entry.
// session_id must be a valid UUID; title must be non-empty after trimming.
func RenameSession(sessionID, title string, opts ...SessionOption) error {
	return session.RenameSession(sessionID, title, opts...)
}

// TagSession tags a local session. Pass clear=true to clear the tag; otherwise
// tag must be non-empty after Unicode sanitization.
func TagSession(sessionID, tag string, clear bool, opts ...SessionOption) error {
	return session.TagSession(sessionID, tag, clear, opts...)
}

// DeleteSession deletes a local session's JSONL file and its subagent
// subdirectory. This is a hard delete.
func DeleteSession(sessionID string, opts ...SessionOption) error {
	return session.DeleteSession(sessionID, opts...)
}

// ForkSession forks a local session into a new branch with fresh UUIDs,
// copying the full transcript.
func ForkSession(sessionID string, opts ...SessionOption) (*ForkSessionResult, error) {
	return session.ForkSession(sessionID, opts...)
}

// ForkSessionAt forks a local session up to upToMessageID (inclusive; empty for
// the full transcript) with an optional explicit title.
func ForkSessionAt(sessionID, upToMessageID, title string, opts ...SessionOption) (*ForkSessionResult, error) {
	return session.ForkSessionAt(sessionID, upToMessageID, title, opts...)
}

// RenameSessionViaStore renames a session by appending a custom-title entry to
// a SessionStore.
func RenameSessionViaStore(ctx context.Context, store SessionStore, sessionID, title, directory string) error {
	return session.RenameSessionViaStore(ctx, store, sessionID, title, directory)
}

// TagSessionViaStore tags a session via a SessionStore. Pass clear=true to
// clear the tag.
func TagSessionViaStore(ctx context.Context, store SessionStore, sessionID, tag, directory string, clear bool) error {
	return session.TagSessionViaStore(ctx, store, sessionID, tag, directory, clear)
}

// DeleteSessionViaStore deletes a session via a SessionStore. No-op if the
// store does not implement SessionDeleter.
func DeleteSessionViaStore(ctx context.Context, store SessionStore, sessionID, directory string) error {
	return session.DeleteSessionViaStore(ctx, store, sessionID, directory)
}

// ForkSessionViaStore forks a session into a new branch with fresh UUIDs via a
// SessionStore. up_to_message_id (empty for full) and title are optional.
func ForkSessionViaStore(ctx context.Context, store SessionStore, sessionID, directory, upToMessageID, title string) (*ForkSessionResult, error) {
	return session.ForkSessionViaStore(ctx, store, sessionID, directory, upToMessageID, title)
}

// ImportSessionToStore replays a local on-disk session transcript into a
// SessionStore (the inverse of resume). directory has the same semantics as
// ListSessions (empty searches all projects). includeSubagents also imports
// subagent transcripts and their metadata sidecars. batchSize<=0 uses the
// default (500 entries per append).
func ImportSessionToStore(ctx context.Context, sessionID string, store SessionStore, directory string, includeSubagents bool, batchSize int) error {
	return session.ImportSessionToStore(ctx, sessionID, store, directory, includeSubagents, batchSize)
}
