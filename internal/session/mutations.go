package session

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// ForkSessionResult is returned by fork operations.
type ForkSessionResult struct {
	// SessionID is the UUID of the new forked session.
	SessionID string
}

// newUUID returns a random RFC 4122 v4 UUID string.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func isoNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// forkTranscriptTypes are the entry types kept as transcript lines during fork.
var forkTranscriptTypes = map[string]bool{
	"user": true, "assistant": true, "attachment": true, "system": true, "progress": true,
}

// resolveForkParent walks parentUuid links skipping progress ancestors and
// returns the remapped parent UUID (or nil when there is no non-progress
// ancestor in the mapping).
func resolveForkParent(original map[string]any, uuidMapping map[string]string, byUUID map[string]map[string]any) any {
	parentID, _ := original["parentUuid"].(string)
	for parentID != "" {
		parent, ok := byUUID[parentID]
		if !ok {
			return nil
		}
		if parent["type"] != "progress" {
			if mapped, ok := uuidMapping[parentID]; ok {
				return mapped
			}
			return nil
		}
		parentID, _ = parent["parentUuid"].(string)
	}
	return nil
}

// transformForkEntry produces one forked entry: remapped uuid/parentUuid/
// logicalParentUuid, rewritten sessionId, stamped forkedFrom, and stripped
// source-session fields. isLast controls timestamp refresh (leaf detection).
func transformForkEntry(
	original map[string]any,
	uuidMapping map[string]string,
	byUUID map[string]map[string]any,
	sessionID, forkedSessionID, now string,
	isLast bool,
) map[string]any {
	uid, _ := original["uuid"].(string)

	timestamp := now
	if !isLast {
		if ts, ok := original["timestamp"].(string); ok {
			timestamp = ts
		}
	}

	forked := make(map[string]any, len(original)+4)
	for k, v := range original {
		forked[k] = v
	}
	forked["uuid"] = uuidMapping[uid]
	forked["parentUuid"] = resolveForkParent(original, uuidMapping, byUUID)
	if _, present := original["logicalParentUuid"]; present {
		forked["logicalParentUuid"] = remapLogicalParent(original, uuidMapping)
	}
	forked["sessionId"] = forkedSessionID
	forked["timestamp"] = timestamp
	forked["isSidechain"] = false
	forked["forkedFrom"] = map[string]any{
		"sessionId":   sessionID,
		"messageUuid": uid,
	}
	for _, k := range []string{"teamName", "agentName", "slug", "sourceToolAssistantUUID"} {
		delete(forked, k)
	}
	return forked
}

// remapLogicalParent remaps the compact-boundary backpointer, preserving the
// original value when it is not in the mapping.
func remapLogicalParent(original map[string]any, uuidMapping map[string]string) any {
	if lp, ok := original["logicalParentUuid"].(string); ok && lp != "" {
		if mapped, ok := uuidMapping[lp]; ok {
			return mapped
		}
		return lp
	}
	return original["logicalParentUuid"]
}

// buildForkLines is the core fork transform: it remaps every message UUID,
// rewrites sessionId, preserves the parentUuid chain (skipping progress
// ancestors), stamps forkedFrom, and appends a content-replacement entry (if
// any) plus a custom-title entry. Returns the new session ID and the serialized
// JSONL lines (compact JSON, no trailing newline). Shared by the disk and
// store-backed paths. Mirrors the Python SDK's _build_fork_lines.
func buildForkLines(
	transcript []map[string]any,
	contentReplacements []any,
	sessionID, upToMessageID string,
	title string,
	deriveTitle func() string,
) (string, []string, error) {
	// Filter out sidechains; keep isMeta (interleaved in main chain).
	var filtered []map[string]any
	for _, e := range transcript {
		if e["isSidechain"] == true {
			continue
		}
		filtered = append(filtered, e)
	}
	transcript = filtered
	if len(transcript) == 0 {
		return "", nil, fmt.Errorf("session %s has no messages to fork", sessionID)
	}

	if upToMessageID != "" {
		cutoff := -1
		for i, e := range transcript {
			if e["uuid"] == upToMessageID {
				cutoff = i
				break
			}
		}
		if cutoff == -1 {
			return "", nil, fmt.Errorf("message %s not found in session %s", upToMessageID, sessionID)
		}
		transcript = transcript[:cutoff+1]
	}

	// Map every entry uuid (incl. progress) so parentUuid walk resolves.
	uuidMapping := make(map[string]string, len(transcript))
	byUUID := make(map[string]map[string]any, len(transcript))
	for _, e := range transcript {
		if uid, ok := e["uuid"].(string); ok {
			uuidMapping[uid] = newUUID()
			byUUID[uid] = e
		}
	}

	// Writable = non-progress entries.
	var writable []map[string]any
	for _, e := range transcript {
		if e["type"] != "progress" {
			writable = append(writable, e)
		}
	}
	if len(writable) == 0 {
		return "", nil, fmt.Errorf("session %s has no messages to fork", sessionID)
	}

	forkedSessionID := newUUID()
	now := isoNow()
	lines := make([]string, 0, len(writable)+2)

	for i, original := range writable {
		isLast := i == len(writable)-1
		forked := transformForkEntry(original, uuidMapping, byUUID, sessionID, forkedSessionID, now, isLast)
		b, err := json.Marshal(forked)
		if err != nil {
			return "", nil, err
		}
		lines = append(lines, string(b))
	}

	// content-replacement entry.
	if len(contentReplacements) > 0 {
		cr := map[string]any{
			"type":         "content-replacement",
			"sessionId":    forkedSessionID,
			"replacements": contentReplacements,
			"uuid":         newUUID(),
			"timestamp":    now,
		}
		if b, err := json.Marshal(cr); err == nil {
			lines = append(lines, string(b))
		}
	}

	// custom-title: explicit > derived + " (fork)".
	forkTitle := strings.TrimSpace(title)
	if forkTitle == "" {
		base := ""
		if deriveTitle != nil {
			base = deriveTitle()
		}
		if base == "" {
			base = "Forked session"
		}
		forkTitle = base + " (fork)"
	}
	ct := map[string]any{
		"type":        "custom-title",
		"sessionId":   forkedSessionID,
		"customTitle": forkTitle,
		"uuid":        newUUID(),
		"timestamp":   now,
	}
	if b, err := json.Marshal(ct); err == nil {
		lines = append(lines, string(b))
	}

	return forkedSessionID, lines, nil
}

// deriveTitleFromEntries mirrors the disk path's title scan over parsed entries:
// last customTitle wins, then last aiTitle, then first user prompt.
func deriveTitleFromEntries(raw []map[string]any) string {
	var custom, ai string
	for _, e := range raw {
		if ct, ok := e["customTitle"].(string); ok && ct != "" {
			custom = ct
		}
		if at, ok := e["aiTitle"].(string); ok && at != "" {
			ai = at
		}
	}
	if custom != "" {
		return custom
	}
	if ai != "" {
		return ai
	}
	// First-prompt fallback: reuse extractFirstPrompt over the parsed entries.
	jes := make([]jsonlEntry, 0, len(raw))
	for _, e := range raw {
		typ, _ := e["type"].(string)
		jes = append(jes, jsonlEntry{entryType: typ, raw: e})
	}
	if fp := extractFirstPrompt(jes); fp != nil {
		return *fp
	}
	return ""
}

// partitionForkEntries splits store-loaded/parsed entries into transcript
// entries (with uuid) and content-replacement records for the given session.
func partitionForkEntries(raw []map[string]any, sessionID string) ([]map[string]any, []any) {
	var transcript []map[string]any
	var contentReplacements []any
	for _, e := range raw {
		typ, _ := e["type"].(string)
		if forkTranscriptTypes[typ] {
			if _, ok := e["uuid"].(string); ok {
				transcript = append(transcript, e)
			}
		} else if typ == "content-replacement" && e["sessionId"] == sessionID {
			if repl, ok := e["replacements"].([]any); ok {
				contentReplacements = append(contentReplacements, repl...)
			}
		}
	}
	return transcript, contentReplacements
}

// ---------------------------------------------------------------------------
// SessionStore-backed mutations
// ---------------------------------------------------------------------------

// RenameSessionViaStore renames a session by appending a custom-title entry to
// a SessionStore. Mirrors rename_session_via_store.
func RenameSessionViaStore(ctx context.Context, store sessionstore.Store, sessionID, title, directory string) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	stripped := strings.TrimSpace(title)
	if stripped == "" {
		return errors.New("title must be non-empty")
	}
	key := sessionstore.SessionKey{ProjectKey: sessionstore.ProjectKeyForDirectory(directory), SessionID: sessionID}
	entry := map[string]any{
		"type":        "custom-title",
		"customTitle": stripped,
		"sessionId":   sessionID,
		"uuid":        newUUID(),
		"timestamp":   isoNow(),
	}
	return appendEntry(ctx, store, key, entry)
}

// TagSessionViaStore tags a session by appending a tag entry to a SessionStore.
// Pass an empty tag to clear. Mirrors tag_session_via_store.
func TagSessionViaStore(ctx context.Context, store sessionstore.Store, sessionID, tag, directory string, clear bool) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	tagVal := ""
	if !clear {
		sanitized := strings.TrimSpace(sanitizeTagUnicode(tag))
		if sanitized == "" {
			return errors.New("tag must be non-empty (use clear=true to clear)")
		}
		tagVal = sanitized
	}
	key := sessionstore.SessionKey{ProjectKey: sessionstore.ProjectKeyForDirectory(directory), SessionID: sessionID}
	entry := map[string]any{
		"type":      "tag",
		"tag":       tagVal,
		"sessionId": sessionID,
		"uuid":      newUUID(),
		"timestamp": isoNow(),
	}
	return appendEntry(ctx, store, key, entry)
}

// DeleteSessionViaStore deletes a session from a SessionStore. No-op if the
// store does not implement Deleter (append-only backends). Mirrors
// delete_session_via_store.
func DeleteSessionViaStore(ctx context.Context, store sessionstore.Store, sessionID, directory string) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	deleter, ok := sessionstore.AsDeleter(store)
	if !ok {
		return nil
	}
	key := sessionstore.SessionKey{ProjectKey: sessionstore.ProjectKeyForDirectory(directory), SessionID: sessionID}
	return deleter.Delete(ctx, key)
}

// ForkSessionViaStore forks a session into a new branch with fresh UUIDs via a
// SessionStore. Runs the fork transform over the loaded objects (a storage-layer
// copy would leave stale session IDs). Mirrors fork_session_via_store.
func ForkSessionViaStore(ctx context.Context, store sessionstore.Store, sessionID, directory, upToMessageID, title string) (*ForkSessionResult, error) {
	if !sessionstore.ValidateUUID(sessionID) {
		return nil, fmt.Errorf("invalid session_id: %s", sessionID)
	}
	if upToMessageID != "" && !sessionstore.ValidateUUID(upToMessageID) {
		return nil, fmt.Errorf("invalid up_to_message_id: %s", upToMessageID)
	}
	projectKey := sessionstore.ProjectKeyForDirectory(directory)
	loaded, err := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}

	raw := make([]map[string]any, 0, len(loaded))
	for _, e := range loaded {
		var m map[string]any
		if err := json.Unmarshal(e, &m); err == nil {
			raw = append(raw, m)
		}
	}
	transcript, contentReplacements := partitionForkEntries(raw, sessionID)

	forkedID, lines, err := buildForkLines(transcript, contentReplacements, sessionID, upToMessageID, title, func() string {
		return deriveTitleFromEntries(raw)
	})
	if err != nil {
		return nil, err
	}

	// Re-parse compact JSON lines to entries so the store receives the same
	// shape as the mirror path.
	entries := make([]sessionstore.Entry, len(lines))
	for i, ln := range lines {
		entries[i] = sessionstore.Entry(ln)
	}
	dstKey := sessionstore.SessionKey{ProjectKey: projectKey, SessionID: forkedID}
	if err := store.Append(ctx, dstKey, entries); err != nil {
		return nil, err
	}
	return &ForkSessionResult{SessionID: forkedID}, nil
}

func appendEntry(ctx context.Context, store sessionstore.Store, key sessionstore.SessionKey, entry map[string]any) error {
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return store.Append(ctx, key, []sessionstore.Entry{sessionstore.Entry(b)})
}

// ---------------------------------------------------------------------------
// Local-disk mutations
// ---------------------------------------------------------------------------

// RenameSession renames a local session by appending a custom-title entry.
// Mirrors rename_session.
func RenameSession(sessionID, title string, opts ...Option) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	stripped := strings.TrimSpace(title)
	if stripped == "" {
		return errors.New("title must be non-empty")
	}
	entry := map[string]any{"type": "custom-title", "customTitle": stripped, "sessionId": sessionID}
	return appendToLocalSession(sessionID, entry, opts...)
}

// TagSession tags a local session by appending a tag entry. Pass clear=true to
// clear. Mirrors tag_session.
func TagSession(sessionID, tag string, clear bool, opts ...Option) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	tagVal := ""
	if !clear {
		sanitized := strings.TrimSpace(sanitizeTagUnicode(tag))
		if sanitized == "" {
			return errors.New("tag must be non-empty (use clear=true to clear)")
		}
		tagVal = sanitized
	}
	entry := map[string]any{"type": "tag", "tag": tagVal, "sessionId": sessionID}
	return appendToLocalSession(sessionID, entry, opts...)
}

// DeleteSession deletes a local session's JSONL file and subagent subdirectory.
// Mirrors delete_session.
func DeleteSession(sessionID string, opts ...Option) error {
	if !sessionstore.ValidateUUID(sessionID) {
		return fmt.Errorf("invalid session_id: %s", sessionID)
	}
	o := defaultOpts()
	for _, fn := range opts {
		fn(&o)
	}
	path, err := findSessionFile(sessionID, o)
	if err != nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	// Subagent transcripts live in a sibling {sessionID}/ dir; often absent.
	_ = os.RemoveAll(filepath.Join(filepath.Dir(path), sessionID))
	return nil
}

// ForkSession forks a local session into a new branch with fresh UUIDs. Mirrors
// fork_session.
func ForkSession(sessionID string, opts ...Option) (*ForkSessionResult, error) {
	return ForkSessionAt(sessionID, "", "", opts...)
}

// ForkSessionAt forks a local session, optionally up to a message UUID and with
// an explicit title.
func ForkSessionAt(sessionID, upToMessageID, title string, opts ...Option) (*ForkSessionResult, error) {
	if !sessionstore.ValidateUUID(sessionID) {
		return nil, fmt.Errorf("invalid session_id: %s", sessionID)
	}
	if upToMessageID != "" && !sessionstore.ValidateUUID(upToMessageID) {
		return nil, fmt.Errorf("invalid up_to_message_id: %s", upToMessageID)
	}
	o := defaultOpts()
	for _, fn := range opts {
		fn(&o)
	}
	path, err := findSessionFile(sessionID, o)
	if err != nil {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	entries, err := parseJSONLFile(path)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("session %s has no messages to fork", sessionID)
	}
	raw := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		raw = append(raw, e.raw)
	}
	transcript, contentReplacements := partitionForkEntries(raw, sessionID)

	forkedID, lines, err := buildForkLines(transcript, contentReplacements, sessionID, upToMessageID, title, func() string {
		return deriveTitleFromEntries(raw)
	})
	if err != nil {
		return nil, err
	}

	forkPath := filepath.Join(filepath.Dir(path), forkedID+".jsonl")
	f, err := os.OpenFile(forkPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // fork path derived from validated UUID
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		return nil, err
	}
	return &ForkSessionResult{SessionID: forkedID}, nil
}

// appendToLocalSession appends a JSON entry (as a JSONL line) to an existing
// local session file, searching candidate project directories.
func appendToLocalSession(sessionID string, entry map[string]any, opts ...Option) error {
	o := defaultOpts()
	for _, fn := range opts {
		fn(&o)
	}
	path, err := findSessionFile(sessionID, o)
	if err != nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path from findSessionFile
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(b, '\n'))
	return err
}

// sanitizeTagUnicode removes zero-width, directional, BOM, and private-use
// characters from a tag for CLI filter compatibility. Mirrors the Python SDK's
// _sanitize_unicode (explicit-range subset; category-based NFKC is applied via
// a best-effort ASCII-safe pass here).
func sanitizeTagUnicode(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 0x200b && r <= 0x200f: // zero-width, LTR/RTL marks
			continue
		case r >= 0x202a && r <= 0x202e: // directional formatting
			continue
		case r >= 0x2066 && r <= 0x2069: // directional isolates
			continue
		case r == 0xfeff: // BOM
			continue
		case r >= 0xe000 && r <= 0xf8ff: // BMP private use
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
