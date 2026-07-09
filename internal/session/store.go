package session

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// storeListLoadConcurrency bounds concurrent per-session loads in the
// list-sessions slow path so large listings don't exhaust adapter pools.
const storeListLoadConcurrency = 16

// entryToJSONLEntry decodes a store Entry (raw JSON bytes) into the internal
// jsonlEntry shape the chain builders operate on. Returns ok=false for
// non-object entries.
func entryToJSONLEntry(e sessionstore.Entry) (jsonlEntry, bool) {
	var raw map[string]any
	if err := json.Unmarshal(e, &raw); err != nil {
		return jsonlEntry{}, false
	}
	typ, _ := raw["type"].(string)
	return jsonlEntry{entryType: typ, raw: raw}, true
}

// storeEntriesToJSONL converts store entries to jsonlEntry values, skipping
// non-objects.
func storeEntriesToJSONL(entries []sessionstore.Entry) []jsonlEntry {
	out := make([]jsonlEntry, 0, len(entries))
	for _, e := range entries {
		if je, ok := entryToJSONLEntry(e); ok {
			out = append(out, je)
		}
	}
	return out
}

// ListSessionsFromStore lists sessions from a SessionStore. It prefers the
// SummaryLister fast path (one batch call + gap-fill), falling back to
// ListSessions + per-session Load. directory computes the project key.
//
// Returns an error if the store implements neither SummaryLister nor
// SessionLister. Mirrors the Python SDK's list_sessions_from_store.
func ListSessionsFromStore(ctx context.Context, store sessionstore.Store, directory string, limit, offset int) ([]SDKSessionInfo, error) {
	projectPath := sessionstore.CanonicalizePath(dirOrDot(directory))
	projectKey := sessionstore.SanitizePath(projectPath)

	summaryLister, hasSummaries := sessionstore.AsSummaryLister(store)
	sessionLister, hasList := sessionstore.AsSessionLister(store)

	if hasSummaries {
		infos, err := listViaSummaries(ctx, store, summaryLister, sessionLister, hasList, projectKey, projectPath, limit, offset)
		if err != nil {
			return nil, err
		}
		return infos, nil
	}

	if !hasList {
		return nil, &StoreCapabilityError{Method: "ListSessions() or ListSessionSummaries()"}
	}

	listing, err := sessionLister.ListSessions(ctx, projectKey)
	if err != nil {
		return nil, err
	}
	results := deriveInfosViaLoad(ctx, store, listing, directory, projectPath)
	return applySortLimitOffset(results, limit, offset), nil
}

// listViaSummaries implements the summary fast path with gap-fill for
// missing/stale sidecars.
func listViaSummaries(
	ctx context.Context,
	store sessionstore.Store,
	summaryLister sessionstore.SummaryLister,
	sessionLister sessionstore.SessionLister,
	hasList bool,
	projectKey, projectPath string,
	limit, offset int,
) ([]SDKSessionInfo, error) {
	summaries, err := summaryLister.ListSessionSummaries(ctx, projectKey)
	if err != nil {
		return nil, err
	}

	var listing []sessionstore.ListEntry
	knownMTimes := map[string]int64{}
	if hasList {
		listing, err = sessionLister.ListSessions(ctx, projectKey)
		if err != nil {
			return nil, err
		}
		for _, e := range listing {
			knownMTimes[e.SessionID] = e.MTime
		}
	}

	slots := buildSummarySlots(summaries, listing, hasList, knownMTimes, projectPath)

	// Paginate BEFORE per-session load so gap-fill load count is bounded by
	// page size, not total missing.
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].mtime > slots[j].mtime })
	if offset > 0 {
		if offset >= len(slots) {
			slots = nil
		} else {
			slots = slots[offset:]
		}
	}
	if limit > 0 && len(slots) > limit {
		slots = slots[:limit]
	}

	gapFillSlots(ctx, store, slots, projectPath)

	out := make([]SDKSessionInfo, 0, len(slots))
	for _, sl := range slots {
		if sl.info != nil {
			out = append(out, *sl.info)
		}
	}
	return out, nil
}

// summarySlot is a candidate session for ListSessionsFromStore: either a
// resolved info (fresh summary) or a placeholder (info nil) to gap-fill by load.
type summarySlot struct {
	mtime     int64
	sessionID string
	info      *SDKSessionInfo
}

// buildSummarySlots turns fresh summaries into resolved slots and lists
// sessions missing/stale sidecars as gap-fill placeholders.
func buildSummarySlots(
	summaries []sessionstore.SummaryEntry,
	listing []sessionstore.ListEntry,
	hasList bool,
	knownMTimes map[string]int64,
	projectPath string,
) []summarySlot {
	var slots []summarySlot
	freshSummaryIDs := map[string]bool{}
	for _, sm := range summaries {
		sid := sm.SessionID
		if hasList {
			known, ok := knownMTimes[sid]
			if !ok {
				continue // summary for a session list no longer reports — drop
			}
			if sm.MTime < known {
				continue // stale sidecar — gap-fill re-folds from source
			}
		}
		info := summaryEntryToSDKInfo(sm, projectPath)
		freshSummaryIDs[sid] = true
		if info == nil {
			continue
		}
		slots = append(slots, summarySlot{mtime: sm.MTime, sessionID: sid, info: info})
	}
	if hasList {
		for _, e := range listing {
			if !freshSummaryIDs[e.SessionID] {
				slots = append(slots, summarySlot{mtime: e.MTime, sessionID: e.SessionID, info: nil})
			}
		}
	}
	return slots
}

// gapFillSlots resolves placeholder slots (info nil) via per-session load,
// mutating slots in place.
func gapFillSlots(ctx context.Context, store sessionstore.Store, slots []summarySlot, projectPath string) {
	var toFill []sessionstore.ListEntry
	for _, sl := range slots {
		if sl.info == nil {
			toFill = append(toFill, sessionstore.ListEntry{SessionID: sl.sessionID, MTime: sl.mtime})
		}
	}
	if len(toFill) == 0 {
		return
	}
	filled := deriveInfosViaLoad(ctx, store, toFill, "", projectPath)
	bySID := map[string]SDKSessionInfo{}
	for _, f := range filled {
		bySID[f.SessionID] = f
	}
	for i := range slots {
		if slots[i].info == nil {
			if f, ok := bySID[slots[i].sessionID]; ok {
				cp := f
				slots[i].info = &cp
			}
		}
	}
}

// deriveInfosViaLoad derives an SDKSessionInfo for each listing entry via
// per-session store.Load, folding entries into a summary. Loads run
// concurrently with a fixed bound. Sidechain / no-summary sessions are dropped.
// directory is used to compute the project key when loading; empty means use
// projectPath's key (already computed by caller via listing).
func deriveInfosViaLoad(ctx context.Context, store sessionstore.Store, listing []sessionstore.ListEntry, directory, projectPath string) []SDKSessionInfo {
	projectKey := sessionstore.SanitizePath(projectPath)
	if directory != "" {
		projectKey = sessionstore.ProjectKeyForDirectory(directory)
	}

	type outcome struct {
		info *SDKSessionInfo
	}
	results := make([]outcome, len(listing))
	sem := make(chan struct{}, storeListLoadConcurrency)
	done := make(chan int, len(listing))

	for i, e := range listing {
		go func(i int, sid string, mtime int64) {
			sem <- struct{}{}
			defer func() { <-sem; done <- i }()
			entries, err := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sid})
			if err != nil {
				// Degrade to an empty summary row (matches Python).
				results[i] = outcome{info: &SDKSessionInfo{SessionID: sid, Summary: "", LastModified: mtime}}
				return
			}
			if len(entries) == 0 {
				return
			}
			summ := sessionstore.FoldSessionSummary(nil, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sid}, entries)
			summ.MTime = mtime
			info := summaryEntryToSDKInfo(summ, projectPath)
			if info == nil {
				return
			}
			info.LastModified = mtime
			results[i] = outcome{info: info}
		}(i, e.SessionID, e.MTime)
	}
	for range listing {
		<-done
	}

	out := make([]SDKSessionInfo, 0, len(listing))
	for _, r := range results {
		if r.info != nil {
			out = append(out, *r.info)
		}
	}
	return out
}

// GetSessionInfoFromStore reads metadata for a single session from a
// SessionStore. Returns nil if not found, session_id invalid, sidechain, or no
// extractable summary. Mirrors get_session_info_from_store.
func GetSessionInfoFromStore(ctx context.Context, store sessionstore.Store, sessionID, directory string) (*SDKSessionInfo, error) {
	if !sessionstore.ValidateUUID(sessionID) {
		return nil, nil
	}
	projectKey := sessionstore.ProjectKeyForDirectory(directory)
	entries, err := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	projectPath := sessionstore.CanonicalizePath(dirOrDot(directory))
	summ := sessionstore.FoldSessionSummary(nil, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID}, entries)
	summ.MTime = mtimeFromEntries(entries)
	return summaryEntryToSDKInfo(summ, projectPath), nil
}

// GetSessionMessagesFromStore reads a session's conversation messages from a
// SessionStore. Mirrors get_session_messages_from_store.
func GetSessionMessagesFromStore(ctx context.Context, store sessionstore.Store, sessionID, directory string, limit, offset int) ([]Message, error) {
	if !sessionstore.ValidateUUID(sessionID) {
		return nil, nil
	}
	projectKey := sessionstore.ProjectKeyForDirectory(directory)
	entries, err := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	messages := buildMessages(sessionID, storeEntriesToJSONL(entries))
	return pageMessages(messages, limit, offset), nil
}

// ListSubagentsFromStore lists subagent IDs for a session via a SessionStore's
// SubkeyLister. Mirrors list_subagents_from_store.
func ListSubagentsFromStore(ctx context.Context, store sessionstore.Store, sessionID, directory string) ([]string, error) {
	if !sessionstore.ValidateUUID(sessionID) {
		return nil, nil
	}
	lister, ok := sessionstore.AsSubkeyLister(store)
	if !ok {
		return nil, &StoreCapabilityError{Method: "ListSubkeys()"}
	}
	projectKey := sessionstore.ProjectKeyForDirectory(directory)
	subkeys, err := lister.ListSubkeys(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var ids []string
	for _, subpath := range subkeys {
		if !strings.HasPrefix(subpath, "subagents/") {
			continue
		}
		last := subpath[strings.LastIndex(subpath, "/")+1:]
		if strings.HasPrefix(last, "agent-") {
			agentID := last[len("agent-"):]
			if !seen[agentID] {
				seen[agentID] = true
				ids = append(ids, agentID)
			}
		}
	}
	return ids, nil
}

// GetSubagentMessagesFromStore reads a subagent's conversation messages from a
// SessionStore. Mirrors get_subagent_messages_from_store.
func GetSubagentMessagesFromStore(ctx context.Context, store sessionstore.Store, sessionID, agentID, directory string, limit, offset int) ([]Message, error) {
	if !sessionstore.ValidateUUID(sessionID) || agentID == "" {
		return nil, nil
	}
	projectKey := sessionstore.ProjectKeyForDirectory(directory)

	subpath := "subagents/agent-" + agentID
	if lister, ok := sessionstore.AsSubkeyLister(store); ok {
		subkeys, err := lister.ListSubkeys(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID})
		if err != nil {
			return nil, err
		}
		target := "agent-" + agentID
		match := ""
		for _, sk := range subkeys {
			if strings.HasPrefix(sk, "subagents/") && sk[strings.LastIndex(sk, "/")+1:] == target {
				match = sk
				break
			}
		}
		if match == "" {
			return nil, nil
		}
		subpath = match
	}

	entries, err := store.Load(ctx, sessionstore.SessionKey{ProjectKey: projectKey, SessionID: sessionID, Subpath: subpath})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	// Drop synthetic agent_metadata entries.
	jes := make([]jsonlEntry, 0, len(entries))
	for _, e := range entries {
		je, ok := entryToJSONLEntry(e)
		if !ok || je.entryType == "agent_metadata" {
			continue
		}
		jes = append(jes, je)
	}
	if len(jes) == 0 {
		return nil, nil
	}
	messages := buildSubagentMessages(sessionID, jes)
	return pageMessages(messages, limit, offset), nil
}

// StoreCapabilityError is returned when a store lacks an optional method
// required by a from-store operation.
type StoreCapabilityError struct {
	Method string
}

func (e *StoreCapabilityError) Error() string {
	return "session_store does not implement " + e.Method
}

// --- shared helpers ---

func dirOrDot(directory string) string {
	if directory == "" {
		return "."
	}
	return directory
}

// mtimeFromEntries returns a best-effort mtime from the last entry's timestamp,
// falling back to 0 (caller may overwrite). Used for from-store info reads.
func mtimeFromEntries(entries []sessionstore.Entry) int64 {
	for i := len(entries) - 1; i >= 0; i-- {
		var m map[string]any
		if err := json.Unmarshal(entries[i], &m); err != nil {
			continue
		}
		if ts, ok := m["timestamp"].(string); ok {
			if ms, ok2 := parseISOms(ts); ok2 {
				return ms
			}
		}
	}
	return 0
}

func parseISOms(ts string) (int64, bool) {
	// Reuse the session package's RFC3339 parsing via time.
	return isoToEpochMSSession(ts)
}

func applySortLimitOffset(sessions []SDKSessionInfo, limit, offset int) []SDKSessionInfo {
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].LastModified > sessions[j].LastModified })
	if offset > 0 {
		if offset >= len(sessions) {
			return nil
		}
		sessions = sessions[offset:]
	}
	if limit > 0 && len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions
}

// pageMessages applies offset+limit with the same semantics as the disk path.
func pageMessages(messages []Message, limit, offset int) []Message {
	if limit > 0 {
		if offset >= len(messages) {
			return nil
		}
		end := offset + limit
		if end > len(messages) {
			end = len(messages)
		}
		return messages[offset:end]
	}
	if offset > 0 {
		if offset >= len(messages) {
			return nil
		}
		return messages[offset:]
	}
	return messages
}
