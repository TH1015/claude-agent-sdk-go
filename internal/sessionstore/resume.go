package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultLoadTimeout bounds SessionStore load/list calls during resume
// materialization when the caller does not set one.
const DefaultLoadTimeout = 30 * time.Second

// uuidRe matches a canonical UUID. Session IDs are used as filesystem path
// components during materialization, so anything that isn't a UUID is rejected
// to prevent traversal.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateUUID returns true if s is a canonical UUID.
func ValidateUUID(s string) bool {
	return uuidRe.MatchString(s)
}

// MaterializedResume is the result of MaterializeResumeSession.
type MaterializedResume struct {
	// ConfigDir is a temporary directory laid out like ~/.claude/. Point the
	// subprocess at it via CLAUDE_CONFIG_DIR.
	ConfigDir string
	// ResumeSessionID is the session ID to pass as --resume. For a
	// continue-conversation input this is the most-recent resolved session.
	ResumeSessionID string
	// ProjectsDir is ConfigDir/projects, for constructing the mirror batcher so
	// file-path → key resolution matches what the subprocess writes.
	ProjectsDir string
}

// Cleanup removes the temporary config dir (best-effort, with retries).
func (m *MaterializedResume) Cleanup() {
	if m == nil || m.ConfigDir == "" {
		return
	}
	rmtreeWithRetry(m.ConfigDir)
}

// MaterializeResumeSession loads a session from store and writes it to a temp
// dir laid out like ~/.claude/, so the CLI subprocess (which only resumes from
// a local file) can resume a session that lives in an external store.
//
// resume is the explicit session ID (empty when not set). continueConversation
// requests resuming the most-recent session. cwd is the working directory used
// to compute the project key. callerEnv is options.ExtraEnv (consulted for
// CLAUDE_CONFIG_DIR when copying auth files).
//
// Returns (nil, nil) when no materialization is needed (no resume/continue,
// store empty, or resolved session ID not a UUID) — the caller falls through
// to the normal no-store path. Returns an error if a store call fails or times
// out. Mirrors the Python SDK's materialize_resume_session.
func MaterializeResumeSession(
	ctx context.Context,
	store Store,
	resume string,
	continueConversation bool,
	cwd string,
	callerEnv map[string]string,
	loadTimeout time.Duration,
) (*MaterializedResume, error) {
	if store == nil {
		return nil, nil
	}
	if resume == "" && !continueConversation {
		return nil, nil
	}
	if loadTimeout <= 0 {
		loadTimeout = DefaultLoadTimeout
	}

	projectKey := ProjectKeyForDirectory(cwd)

	var sessionID string
	var entries []Entry
	var err error
	if resume != "" {
		if !ValidateUUID(resume) {
			return nil, nil
		}
		sessionID, entries, err = loadCandidate(ctx, store, projectKey, resume, loadTimeout)
	} else {
		sessionID, entries, err = resolveContinueCandidate(ctx, store, projectKey, loadTimeout)
	}
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, nil
	}

	tmpBase, err := os.MkdirTemp("", "claude-resume-")
	if err != nil {
		return nil, fmt.Errorf("creating temp resume dir: %w", err)
	}

	if err := materializeInto(ctx, store, tmpBase, projectKey, sessionID, entries, callerEnv, loadTimeout); err != nil {
		rmtreeWithRetry(tmpBase)
		return nil, err
	}

	return &MaterializedResume{
		ConfigDir:       tmpBase,
		ResumeSessionID: sessionID,
		ProjectsDir:     filepath.Join(tmpBase, "projects"),
	}, nil
}

// materializeInto writes the main transcript, copies auth files, and (when the
// store supports it) materializes subagent transcripts.
func materializeInto(
	ctx context.Context,
	store Store,
	tmpBase, projectKey, sessionID string,
	entries []Entry,
	callerEnv map[string]string,
	loadTimeout time.Duration,
) error {
	projectDir := filepath.Join(tmpBase, "projects", projectKey)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return fmt.Errorf("creating project dir: %w", err)
	}
	if err := writeJSONL(filepath.Join(projectDir, sessionID+".jsonl"), entries); err != nil {
		return err
	}

	copyAuthFiles(tmpBase, callerEnv)

	if subLister, ok := AsSubkeyLister(store); ok {
		if err := materializeSubkeys(ctx, store, subLister, projectDir, projectKey, sessionID, loadTimeout); err != nil {
			return err
		}
	}
	return nil
}

// loadCandidate loads entries for sessionID; returns (id, nil, nil) if empty.
func loadCandidate(ctx context.Context, store Store, projectKey, sessionID string, timeout time.Duration) (string, []Entry, error) {
	entries, err := withTimeout(ctx, timeout, func(c context.Context) ([]Entry, error) {
		return store.Load(c, SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	}, fmt.Sprintf("SessionStore.Load() for session %s", sessionID))
	if err != nil {
		return "", nil, err
	}
	if len(entries) == 0 {
		return "", nil, nil
	}
	return sessionID, entries, nil
}

// resolveContinueCandidate picks the most-recently-modified non-sidechain
// session. Requires the store to implement SessionLister; returns (,, nil) when
// it doesn't (fresh session, matching CLI --continue with no history).
func resolveContinueCandidate(ctx context.Context, store Store, projectKey string, timeout time.Duration) (string, []Entry, error) {
	lister, ok := AsSessionLister(store)
	if !ok {
		return "", nil, nil
	}
	sessions, err := withTimeoutList(ctx, timeout, func(c context.Context) ([]ListEntry, error) {
		return lister.ListSessions(c, projectKey)
	}, "SessionStore.ListSessions()")
	if err != nil {
		return "", nil, err
	}
	if len(sessions) == 0 {
		return "", nil, nil
	}
	// Walk newest→oldest, skipping sidechains.
	sorted := append([]ListEntry(nil), sessions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].MTime > sorted[j].MTime })
	for _, cand := range sorted {
		if !ValidateUUID(cand.SessionID) {
			continue
		}
		id, entries, err := loadCandidate(ctx, store, projectKey, cand.SessionID, timeout)
		if err != nil {
			return "", nil, err
		}
		if entries == nil {
			continue
		}
		if isSidechainEntry(entries[0]) {
			continue
		}
		return id, entries, nil
	}
	return "", nil, nil
}

func isSidechainEntry(e Entry) bool {
	var m map[string]any
	if err := json.Unmarshal(e, &m); err != nil {
		return false
	}
	return m["isSidechain"] == true
}

// withTimeout runs fn with a timeout, re-wrapping timeout/errors with context.
func withTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) ([]Entry, error), what string) ([]Entry, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	entries, err := fn(c)
	if err != nil {
		if errors.Is(c.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %dms during resume materialization", what, timeout.Milliseconds())
		}
		return nil, fmt.Errorf("%s failed during resume materialization: %w", what, err)
	}
	return entries, nil
}

func withTimeoutList(ctx context.Context, timeout time.Duration, fn func(context.Context) ([]ListEntry, error), what string) ([]ListEntry, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	entries, err := fn(c)
	if err != nil {
		if errors.Is(c.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %dms during resume materialization", what, timeout.Milliseconds())
		}
		return nil, fmt.Errorf("%s failed during resume materialization: %w", what, err)
	}
	return entries, nil
}

func withTimeoutSubkeys(ctx context.Context, timeout time.Duration, fn func(context.Context) ([]string, error), what string) ([]string, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := fn(c)
	if err != nil {
		if errors.Is(c.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %dms during resume materialization", what, timeout.Milliseconds())
		}
		return nil, fmt.Errorf("%s failed during resume materialization: %w", what, err)
	}
	return out, nil
}

// writeJSONL writes entries as one compact JSON line each (mode 0600).
func writeJSONL(path string, entries []Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // transcript path derived from validated UUID/project key
	if err != nil {
		return fmt.Errorf("writing transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, e := range entries {
		// e is already compact JSON bytes; write verbatim + newline.
		if _, err := f.Write(e); err != nil {
			return err
		}
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return nil
}

// copyAuthFiles copies .credentials.json (refreshToken redacted) and
// .claude.json from the caller's effective config location so the resumed
// subprocess (running under a redirected CLAUDE_CONFIG_DIR) can authenticate.
// Missing files are fine (API-key auth, etc.). macOS Keychain auth is not
// handled here (best-effort parity; Linux/CI is the primary multi-host target).
func copyAuthFiles(tmpBase string, callerEnv map[string]string) {
	callerConfigDir := ""
	if callerEnv != nil {
		callerConfigDir = callerEnv["CLAUDE_CONFIG_DIR"]
	}
	if callerConfigDir == "" {
		callerConfigDir = os.Getenv("CLAUDE_CONFIG_DIR")
	}
	var sourceConfigDir string
	if callerConfigDir != "" {
		sourceConfigDir = callerConfigDir
	} else if home, err := os.UserHomeDir(); err == nil {
		sourceConfigDir = filepath.Join(home, ".claude")
	}

	if sourceConfigDir != "" {
		if creds, err := os.ReadFile(filepath.Join(sourceConfigDir, ".credentials.json")); err == nil {
			writeRedactedCredentials(creds, filepath.Join(tmpBase, ".credentials.json"))
		}
	}

	// .claude.json lives at $CLAUDE_CONFIG_DIR/.claude.json when set, else
	// ~/.claude.json (NOT ~/.claude/.claude.json).
	var claudeJSONSrc string
	if callerConfigDir != "" {
		claudeJSONSrc = filepath.Join(callerConfigDir, ".claude.json")
	} else if home, err := os.UserHomeDir(); err == nil {
		claudeJSONSrc = filepath.Join(home, ".claude.json")
	}
	if claudeJSONSrc != "" {
		copyIfPresent(claudeJSONSrc, filepath.Join(tmpBase, ".claude.json"))
	}
}

// writeRedactedCredentials writes creds with claudeAiOauth.refreshToken removed
// so the resumed subprocess can't consume the single-use refresh token and
// revoke the parent's stored creds.
func writeRedactedCredentials(creds []byte, dst string) {
	out := creds
	var data map[string]any
	if err := json.Unmarshal(creds, &data); err == nil {
		if oauth, ok := data["claudeAiOauth"].(map[string]any); ok {
			if _, present := oauth["refreshToken"]; present {
				delete(oauth, "refreshToken")
				if b, err := json.Marshal(data); err == nil {
					out = b
				}
			}
		}
	}
	_ = os.WriteFile(dst, out, 0o600)
}

func copyIfPresent(src, dst string) {
	data, err := os.ReadFile(src) //nolint:gosec // config path from caller env / home dir
	if err != nil {
		return
	}
	_ = os.WriteFile(dst, data, 0o600)
}

// materializeSubkeys loads and writes all subagent transcripts/metadata under
// sessionID.
func materializeSubkeys(
	ctx context.Context,
	store Store,
	lister SubkeyLister,
	projectDir, projectKey, sessionID string,
	timeout time.Duration,
) error {
	sessionDir := filepath.Join(projectDir, sessionID)
	subkeys, err := withTimeoutSubkeys(ctx, timeout, func(c context.Context) ([]string, error) {
		return lister.ListSubkeys(c, SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	}, fmt.Sprintf("SessionStore.ListSubkeys() for session %s", sessionID))
	if err != nil {
		return err
	}

	for _, subpath := range subkeys {
		if !isSafeSubpath(subpath, sessionDir) {
			continue
		}
		subEntries, err := withTimeout(ctx, timeout, func(c context.Context) ([]Entry, error) {
			return store.Load(c, SessionKey{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath})
		}, fmt.Sprintf("SessionStore.Load() for session %s subpath %s", sessionID, subpath))
		if err != nil {
			return err
		}
		if len(subEntries) == 0 {
			continue
		}

		// Partition metadata (agent_metadata) from transcript lines.
		var metadata []map[string]any
		var transcript []Entry
		for _, e := range subEntries {
			var m map[string]any
			if err := json.Unmarshal(e, &m); err == nil && m["type"] == "agent_metadata" {
				metadata = append(metadata, m)
			} else {
				transcript = append(transcript, e)
			}
		}

		// The subpath maps to <sessionDir>/<subpath>.jsonl
		subFile := filepath.Join(sessionDir, filepath.FromSlash(subpath)) + ".jsonl"
		if len(transcript) > 0 {
			if err := writeJSONL(subFile, transcript); err != nil {
				return err
			}
		}
		if len(metadata) > 0 {
			// Last metadata entry wins; strip the synthetic "type" field.
			last := metadata[len(metadata)-1]
			meta := make(map[string]any, len(last))
			for k, v := range last {
				if k != "type" {
					meta[k] = v
				}
			}
			metaFile := strings.TrimSuffix(subFile, ".jsonl") + ".meta.json"
			if err := os.MkdirAll(filepath.Dir(metaFile), 0o700); err != nil {
				return err
			}
			if b, err := json.Marshal(meta); err == nil {
				_ = os.WriteFile(metaFile, b, 0o600)
			}
		}
	}
	return nil
}

// isSafeSubpath rejects subpaths that are empty, absolute, contain "..", or
// escape sessionDir after resolution.
func isSafeSubpath(subpath, sessionDir string) bool {
	if subpath == "" {
		return false
	}
	if strings.HasPrefix(subpath, "/") || strings.HasPrefix(subpath, "\\") {
		return false
	}
	if filepath.IsAbs(subpath) {
		return false
	}
	for _, p := range strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if p == "." || p == ".." {
			return false
		}
	}
	if strings.ContainsRune(subpath, '\x00') {
		return false
	}
	target := filepath.Join(sessionDir, filepath.FromSlash(subpath)) + ".jsonl"
	rel, err := filepath.Rel(sessionDir, target)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// rmtreeWithRetry is a best-effort RemoveAll with a few retries for transiently
// held handles. Never panics.
func rmtreeWithRetry(path string) {
	for i := 0; i < 4; i++ {
		if err := os.RemoveAll(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.RemoveAll(path)
}
