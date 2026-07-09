package sessionstore

import (
	"context"
	"strings"
	"sync"
	"time"
)

// InMemoryStore is a reference Store implementation for testing and
// development. Entries live in a map keyed by a composite
// "projectKey/sessionID" string (with an optional "/subpath" suffix). Data is
// lost when the process exits — not suitable for production.
//
// It implements every optional capability (SessionLister, Deleter,
// SubkeyLister, SummaryLister). All access is guarded by a mutex since, unlike
// the single-threaded async Python original, Go callers may append
// concurrently. Mirrors the Python SDK's InMemorySessionStore.
type InMemoryStore struct {
	mu        sync.Mutex
	store     map[string][]Entry
	mtimes    map[string]int64
	summaries map[string]SummaryEntry // keyed by "projectKey\x00sessionID"
	lastMTime int64
}

// NewInMemoryStore creates an empty in-memory session store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{
		store:     make(map[string][]Entry),
		mtimes:    make(map[string]int64),
		summaries: make(map[string]SummaryEntry),
	}
}

// Compile-time assertions that InMemoryStore satisfies every capability.
var (
	_ Store         = (*InMemoryStore)(nil)
	_ SessionLister = (*InMemoryStore)(nil)
	_ Deleter       = (*InMemoryStore)(nil)
	_ SubkeyLister  = (*InMemoryStore)(nil)
	_ SummaryLister = (*InMemoryStore)(nil)
)

func keyToString(key SessionKey) string {
	if key.Subpath != "" {
		return key.ProjectKey + "/" + key.SessionID + "/" + key.Subpath
	}
	return key.ProjectKey + "/" + key.SessionID
}

func summaryMapKey(projectKey, sessionID string) string {
	return projectKey + "\x00" + sessionID
}

// nextMTime returns a strictly monotonically increasing Unix epoch ms value so
// back-to-back appends always produce distinct mtimes (real backends get this
// from commit ordering). Matches the Python SDK's _next_mtime.
func (s *InMemoryStore) nextMTime() int64 {
	nowMS := time.Now().UnixMilli()
	if nowMS <= s.lastMTime {
		nowMS = s.lastMTime + 1
	}
	s.lastMTime = nowMS
	return nowMS
}

// Append persists entries under key and, for main keys, folds them into the
// summary sidecar.
func (s *InMemoryStore) Append(_ context.Context, key SessionKey, entries []Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := keyToString(key)
	// Copy the incoming slice so the caller can't mutate our stored entries.
	cp := make([]Entry, len(entries))
	for i, e := range entries {
		buf := make([]byte, len(e))
		copy(buf, e)
		cp[i] = buf
	}
	s.store[k] = append(s.store[k], cp...)
	nowMS := s.nextMTime()

	// Maintain the per-session summary sidecar incrementally. Subagent subpaths
	// don't contribute to the main session's summary.
	if key.Subpath == "" {
		sk := summaryMapKey(key.ProjectKey, key.SessionID)
		var prev *SummaryEntry
		if existing, ok := s.summaries[sk]; ok {
			prev = &existing
		}
		folded := FoldSessionSummary(prev, key, entries)
		folded.MTime = nowMS
		s.summaries[sk] = folded
	}
	s.mtimes[k] = nowMS
	return nil
}

// Load returns a copy of the entries stored under key, or (nil, nil) when
// unknown.
func (s *InMemoryStore) Load(_ context.Context, key SessionKey) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, ok := s.store[keyToString(key)]
	if !ok {
		return nil, nil
	}
	out := make([]Entry, len(entries))
	for i, e := range entries {
		buf := make([]byte, len(e))
		copy(buf, e)
		out[i] = buf
	}
	return out, nil
}

// ListSessions returns main-transcript session IDs for a project.
func (s *InMemoryStore) ListSessions(_ context.Context, projectKey string) ([]ListEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := projectKey + "/"
	var results []ListEntry
	for k := range s.store {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		// Only main transcripts (no subpath → no further '/').
		if !strings.Contains(rest, "/") {
			results = append(results, ListEntry{SessionID: rest, MTime: s.mtimes[k]})
		}
	}
	return results, nil
}

// ListSessionSummaries returns the summary sidecars for a project.
func (s *InMemoryStore) ListSessionSummaries(_ context.Context, projectKey string) ([]SummaryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []SummaryEntry
	for mk, summ := range s.summaries {
		pk := mk[:strings.IndexByte(mk, '\x00')]
		if pk == projectKey {
			out = append(out, cloneSummary(summ))
		}
	}
	return out, nil
}

// Delete removes a record. Deleting a main key cascades to its subkeys.
func (s *InMemoryStore) Delete(_ context.Context, key SessionKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := keyToString(key)
	delete(s.store, k)
	delete(s.mtimes, k)

	if key.Subpath == "" {
		delete(s.summaries, summaryMapKey(key.ProjectKey, key.SessionID))
		prefix := key.ProjectKey + "/" + key.SessionID + "/"
		for sk := range s.store {
			if strings.HasPrefix(sk, prefix) {
				delete(s.store, sk)
				delete(s.mtimes, sk)
			}
		}
	}
	return nil
}

// ListSubkeys returns the subpaths stored under a session (excludes the main
// transcript).
func (s *InMemoryStore) ListSubkeys(_ context.Context, key SessionKey) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	prefix := key.ProjectKey + "/" + key.SessionID + "/"
	var out []string
	for k := range s.store {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k[len(prefix):])
		}
	}
	return out, nil
}

// cloneSummary deep-copies a SummaryEntry so callers can't mutate internal state.
func cloneSummary(s SummaryEntry) SummaryEntry {
	return SummaryEntry{SessionID: s.SessionID, MTime: s.MTime, Data: cloneData(s.Data)}
}
