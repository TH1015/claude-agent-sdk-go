// Package conformance provides a shared behavioral test suite for SessionStore
// adapters. Call RunConformance from a *testing.T test to assert the contracts
// every adapter must satisfy. Contracts for optional capabilities
// (SessionLister, SummaryLister, Deleter, SubkeyLister) are skipped
// automatically when the store does not implement them.
//
// Example:
//
//	func TestMyStoreConformance(t *testing.T) {
//	    conformance.RunConformance(t, func() sessionstore.Store {
//	        return NewMyStore()
//	    })
//	}
package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

var mainKey = sessionstore.SessionKey{ProjectKey: "proj", SessionID: "sess"}

// RunConformance asserts the SessionStore behavioral contracts. makeStore is
// invoked once per contract to provide isolation between checks.
func RunConformance(t *testing.T, makeStore func() sessionstore.Store) {
	t.Helper()
	ctx := context.Background()

	probe := makeStore()
	_, hasListSessions := sessionstore.AsSessionLister(probe)
	_, hasSummaries := sessionstore.AsSummaryLister(probe)
	_, hasDelete := sessionstore.AsDeleter(probe)
	_, hasSubkeys := sessionstore.AsSubkeyLister(probe)

	// --- Required: append + load ------------------------------------------

	t.Run("append_then_load_same_order", func(t *testing.T) {
		st := makeStore()
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"b","n":1}`, `{"uuid":"a","n":2}`)))
		loaded, err := st.Load(ctx, mainKey)
		must(t, err)
		assertEntriesEqual(t, loaded, entries(`{"uuid":"b","n":1}`, `{"uuid":"a","n":2}`))
	})

	t.Run("load_unknown_returns_nil", func(t *testing.T) {
		st := makeStore()
		loaded, err := st.Load(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "nope"})
		must(t, err)
		if loaded != nil {
			t.Fatalf("expected nil for unknown key, got %v", loaded)
		}
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"x","n":1}`)))
		sub := mainKey
		sub.Subpath = "nope"
		loaded, err = st.Load(ctx, sub)
		must(t, err)
		if loaded != nil {
			t.Fatalf("expected nil for unknown subpath, got %v", loaded)
		}
	})

	t.Run("multiple_appends_preserve_order", func(t *testing.T) {
		st := makeStore()
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"z","n":1}`)))
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"a","n":2}`, `{"uuid":"m","n":3}`)))
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"b","n":4}`)))
		loaded, err := st.Load(ctx, mainKey)
		must(t, err)
		assertEntriesEqual(t, loaded, entries(
			`{"uuid":"z","n":1}`, `{"uuid":"a","n":2}`, `{"uuid":"m","n":3}`, `{"uuid":"b","n":4}`))
	})

	t.Run("append_empty_is_noop", func(t *testing.T) {
		st := makeStore()
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"a","n":1}`)))
		must(t, st.Append(ctx, mainKey, nil))
		loaded, err := st.Load(ctx, mainKey)
		must(t, err)
		assertEntriesEqual(t, loaded, entries(`{"uuid":"a","n":1}`))
	})

	t.Run("subpath_stored_independently", func(t *testing.T) {
		st := makeStore()
		sub := mainKey
		sub.Subpath = "subagents/agent-1"
		must(t, st.Append(ctx, mainKey, entries(`{"uuid":"m","n":1}`)))
		must(t, st.Append(ctx, sub, entries(`{"uuid":"s","n":1}`)))
		lm, err := st.Load(ctx, mainKey)
		must(t, err)
		assertEntriesEqual(t, lm, entries(`{"uuid":"m","n":1}`))
		ls, err := st.Load(ctx, sub)
		must(t, err)
		assertEntriesEqual(t, ls, entries(`{"uuid":"s","n":1}`))
	})

	t.Run("project_key_isolation", func(t *testing.T) {
		st := makeStore()
		ka := sessionstore.SessionKey{ProjectKey: "A", SessionID: "s1"}
		kb := sessionstore.SessionKey{ProjectKey: "B", SessionID: "s1"}
		must(t, st.Append(ctx, ka, entries(`{"from":"A"}`)))
		must(t, st.Append(ctx, kb, entries(`{"from":"B"}`)))
		la, err := st.Load(ctx, ka)
		must(t, err)
		assertEntriesEqual(t, la, entries(`{"from":"A"}`))
		lb, err := st.Load(ctx, kb)
		must(t, err)
		assertEntriesEqual(t, lb, entries(`{"from":"B"}`))
		if l, ok := sessionstore.AsSessionLister(st); ok {
			assertListLen(t, ctx, l, "A", 1)
			assertListLen(t, ctx, l, "B", 1)
		}
	})

	// --- Optional: list_sessions ------------------------------------------

	if hasListSessions {
		t.Run("list_sessions_returns_ids", func(t *testing.T) {
			st := makeStore()
			l, _ := sessionstore.AsSessionLister(st)
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "a"}, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "b"}, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: "other", SessionID: "c"}, entries(`{"n":1}`)))
			got, err := l.ListSessions(ctx, "proj")
			must(t, err)
			ids := sessionIDs(got)
			sort.Strings(ids)
			if !reflect.DeepEqual(ids, []string{"a", "b"}) {
				t.Fatalf("list_sessions ids = %v, want [a b]", ids)
			}
			for _, s := range got {
				// mtime must be epoch-ms; >1e12 rules out epoch-seconds.
				if s.MTime <= 1_000_000_000_000 {
					t.Fatalf("mtime %d not epoch-ms", s.MTime)
				}
			}
			empty, err := l.ListSessions(ctx, "never-appended-project")
			must(t, err)
			if len(empty) != 0 {
				t.Fatalf("expected empty list, got %v", empty)
			}
		})

		t.Run("list_sessions_excludes_subagents", func(t *testing.T) {
			st := makeStore()
			l, _ := sessionstore.AsSessionLister(st)
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "main"}, entries(`{"n":1}`)))
			sub := sessionstore.SessionKey{ProjectKey: "proj", SessionID: "main", Subpath: "subagents/agent-1"}
			must(t, st.Append(ctx, sub, entries(`{"n":1}`)))
			got, err := l.ListSessions(ctx, "proj")
			must(t, err)
			ids := sessionIDs(got)
			if !reflect.DeepEqual(ids, []string{"main"}) {
				t.Fatalf("list_sessions ids = %v, want [main]", ids)
			}
		})
	}

	// --- Optional: list_session_summaries ---------------------------------

	if hasSummaries {
		t.Run("list_session_summaries_round_trip", func(t *testing.T) {
			st := makeStore()
			sl, _ := sessionstore.AsSummaryLister(st)
			key := sessionstore.SessionKey{ProjectKey: "proj", SessionID: "summ-sess"}
			must(t, st.Append(ctx, key, entries(
				`{"type":"x","timestamp":"2024-01-01T00:00:00.000Z","customTitle":"first"}`,
				`{"type":"x","timestamp":"2024-01-01T00:00:01.000Z"}`)))
			must(t, st.Append(ctx, key, entries(
				`{"type":"x","timestamp":"2024-01-01T00:00:02.000Z","customTitle":"second"}`)))
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: "other", SessionID: "elsewhere"}, entries(
				`{"type":"x","timestamp":"2024-01-01T00:00:00.000Z"}`)))
			summaries, err := sl.ListSessionSummaries(ctx, "proj")
			must(t, err)
			byID := map[string]sessionstore.SummaryEntry{}
			for _, s := range summaries {
				byID[s.SessionID] = s
			}
			if len(byID) != 1 {
				t.Fatalf("expected 1 summary, got %d", len(byID))
			}
			summ, ok := byID["summ-sess"]
			if !ok {
				t.Fatal("missing summ-sess summary")
			}
			if summ.MTime <= 1_000_000_000_000 {
				t.Fatalf("summary mtime %d not epoch-ms", summ.MTime)
			}
			// Clock alignment: sidecar mtime >= list_sessions mtime.
			if l, ok := sessionstore.AsSessionLister(st); ok {
				listing, err := l.ListSessions(ctx, "proj")
				must(t, err)
				for _, e := range listing {
					if e.SessionID == "summ-sess" && summ.MTime < e.MTime {
						t.Fatalf("summary mtime %d < list_sessions mtime %d", summ.MTime, e.MTime)
					}
				}
			}
			// data round-trips through the fold verbatim.
			refolded := sessionstore.FoldSessionSummary(&summ, key, entries(
				`{"type":"x","timestamp":"2024-01-01T00:00:03.000Z"}`))
			if refolded.SessionID != "summ-sess" {
				t.Fatalf("refolded session id = %q", refolded.SessionID)
			}
			if refolded.MTime != summ.MTime {
				t.Fatalf("refold changed mtime: %d != %d", refolded.MTime, summ.MTime)
			}
			// Subagent appends must not affect the main summary.
			subKey := key
			subKey.Subpath = "subagents/agent-1"
			must(t, st.Append(ctx, subKey, entries(
				`{"type":"x","timestamp":"2024-01-01T00:00:09.000Z","customTitle":"subagent"}`)))
			after, err := sl.ListSessionSummaries(ctx, "proj")
			must(t, err)
			for _, s := range after {
				if s.SessionID == "summ-sess" && !reflect.DeepEqual(s.Data, summ.Data) {
					t.Fatalf("subagent append changed main summary data")
				}
			}
			none, err := sl.ListSessionSummaries(ctx, "never-appended-project")
			must(t, err)
			if len(none) != 0 {
				t.Fatalf("expected empty summaries, got %v", none)
			}
			if d, ok := sessionstore.AsDeleter(st); ok {
				must(t, d.Delete(ctx, key))
				remaining, err := sl.ListSessionSummaries(ctx, "proj")
				must(t, err)
				if len(remaining) != 0 {
					t.Fatalf("expected no summaries after delete, got %v", remaining)
				}
			}
		})
	}

	// --- Optional: delete -------------------------------------------------

	if hasDelete {
		t.Run("delete_main_then_load_nil", func(t *testing.T) {
			st := makeStore()
			d, _ := sessionstore.AsDeleter(st)
			must(t, d.Delete(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "never-written"}))
			must(t, st.Append(ctx, mainKey, entries(`{"n":1}`)))
			must(t, d.Delete(ctx, mainKey))
			loaded, err := st.Load(ctx, mainKey)
			must(t, err)
			if loaded != nil {
				t.Fatalf("expected nil after delete, got %v", loaded)
			}
		})

		t.Run("delete_main_cascades_subkeys", func(t *testing.T) {
			st := makeStore()
			d, _ := sessionstore.AsDeleter(st)
			sub1 := mainKey
			sub1.Subpath = "subagents/agent-1"
			sub2 := mainKey
			sub2.Subpath = "subagents/agent-2"
			other := sessionstore.SessionKey{ProjectKey: "proj", SessionID: "sess2"}
			otherProj := sessionstore.SessionKey{ProjectKey: "other-proj", SessionID: mainKey.SessionID}
			must(t, st.Append(ctx, mainKey, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sub1, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sub2, entries(`{"n":1}`)))
			must(t, st.Append(ctx, other, entries(`{"n":1}`)))
			must(t, st.Append(ctx, otherProj, entries(`{"n":1}`)))

			must(t, d.Delete(ctx, mainKey))

			assertNil(t, ctx, st, mainKey)
			assertNil(t, ctx, st, sub1)
			assertNil(t, ctx, st, sub2)
			assertLen(t, ctx, st, other, 1)
			assertLen(t, ctx, st, otherProj, 1)
			if l, ok := sessionstore.AsSubkeyLister(st); ok {
				sk, err := l.ListSubkeys(ctx, mainKey)
				must(t, err)
				if len(sk) != 0 {
					t.Fatalf("expected no subkeys after cascade, got %v", sk)
				}
			}
		})

		t.Run("delete_subpath_removes_only_that", func(t *testing.T) {
			st := makeStore()
			d, _ := sessionstore.AsDeleter(st)
			sub1 := mainKey
			sub1.Subpath = "subagents/agent-1"
			sub2 := mainKey
			sub2.Subpath = "subagents/agent-2"
			must(t, st.Append(ctx, mainKey, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sub1, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sub2, entries(`{"n":1}`)))

			must(t, d.Delete(ctx, sub1))

			assertNil(t, ctx, st, sub1)
			assertLen(t, ctx, st, sub2, 1)
			assertLen(t, ctx, st, mainKey, 1)
			if l, ok := sessionstore.AsSubkeyLister(st); ok {
				sk, err := l.ListSubkeys(ctx, mainKey)
				must(t, err)
				sort.Strings(sk)
				if !reflect.DeepEqual(sk, []string{"subagents/agent-2"}) {
					t.Fatalf("subkeys after targeted delete = %v", sk)
				}
			}
		})
	}

	// --- Optional: list_subkeys -------------------------------------------

	if hasSubkeys {
		t.Run("list_subkeys_returns_subpaths", func(t *testing.T) {
			st := makeStore()
			l, _ := sessionstore.AsSubkeyLister(st)
			must(t, st.Append(ctx, mainKey, entries(`{"n":1}`)))
			s1 := mainKey
			s1.Subpath = "subagents/agent-1"
			s2 := mainKey
			s2.Subpath = "subagents/agent-2"
			must(t, st.Append(ctx, s1, entries(`{"n":1}`)))
			must(t, st.Append(ctx, s2, entries(`{"n":1}`)))
			must(t, st.Append(ctx, sessionstore.SessionKey{ProjectKey: mainKey.ProjectKey, SessionID: "other-sess", Subpath: "subagents/agent-x"}, entries(`{"n":1}`)))
			sk, err := l.ListSubkeys(ctx, mainKey)
			must(t, err)
			sort.Strings(sk)
			if !reflect.DeepEqual(sk, []string{"subagents/agent-1", "subagents/agent-2"}) {
				t.Fatalf("subkeys = %v", sk)
			}
		})

		t.Run("list_subkeys_excludes_main", func(t *testing.T) {
			st := makeStore()
			l, _ := sessionstore.AsSubkeyLister(st)
			must(t, st.Append(ctx, mainKey, entries(`{"n":1}`)))
			sk, err := l.ListSubkeys(ctx, mainKey)
			must(t, err)
			if len(sk) != 0 {
				t.Fatalf("expected no subkeys, got %v", sk)
			}
			sk, err = l.ListSubkeys(ctx, sessionstore.SessionKey{ProjectKey: "proj", SessionID: "never-appended"})
			must(t, err)
			if len(sk) != 0 {
				t.Fatalf("expected no subkeys for unknown session, got %v", sk)
			}
		})
	}
}

// --- helpers -----------------------------------------------------------------

func entries(jsons ...string) []sessionstore.Entry {
	out := make([]sessionstore.Entry, len(jsons))
	for i, j := range jsons {
		out[i] = sessionstore.Entry(j)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// assertEntriesEqual compares two entry slices by deep JSON equality (not byte
// equality — matching the SessionStore contract).
func assertEntriesEqual(t *testing.T, got, want []sessionstore.Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("entry count %d != %d (got %s)", len(got), len(want), string(joinEntries(got)))
	}
	for i := range got {
		var g, w any
		if err := json.Unmarshal(got[i], &g); err != nil {
			t.Fatalf("unmarshal got[%d]: %v", i, err)
		}
		if err := json.Unmarshal(want[i], &w); err != nil {
			t.Fatalf("unmarshal want[%d]: %v", i, err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("entry[%d] = %v, want %v", i, g, w)
		}
	}
}

func joinEntries(es []sessionstore.Entry) []byte {
	var out []byte
	for _, e := range es {
		out = append(out, e...)
		out = append(out, '\n')
	}
	return out
}

func sessionIDs(es []sessionstore.ListEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.SessionID
	}
	return out
}

func assertListLen(t *testing.T, ctx context.Context, l sessionstore.SessionLister, projectKey string, want int) {
	t.Helper()
	got, err := l.ListSessions(ctx, projectKey)
	must(t, err)
	if len(got) != want {
		t.Fatalf("list_sessions(%q) len = %d, want %d", projectKey, len(got), want)
	}
}

func assertNil(t *testing.T, ctx context.Context, st sessionstore.Store, key sessionstore.SessionKey) {
	t.Helper()
	loaded, err := st.Load(ctx, key)
	must(t, err)
	if loaded != nil {
		t.Fatalf("expected nil for %+v, got %v", key, loaded)
	}
}

func assertLen(t *testing.T, ctx context.Context, st sessionstore.Store, key sessionstore.SessionKey, want int) {
	t.Helper()
	loaded, err := st.Load(ctx, key)
	must(t, err)
	if len(loaded) != want {
		t.Fatalf("load %+v len = %d, want %d", key, len(loaded), want)
	}
}
