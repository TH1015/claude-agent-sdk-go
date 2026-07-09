package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingStore captures Append calls and can be programmed to fail.
type recordingStore struct {
	mu       sync.Mutex
	appends  []appendCall
	failN    int32 // fail the first N append attempts (across all keys)
	failWith error
	blockFor time.Duration // sleep inside Append to simulate a slow/timeout backend
}

type appendCall struct {
	key     SessionKey
	entries []Entry
}

func (s *recordingStore) Append(ctx context.Context, key SessionKey, entries []Entry) error {
	if s.blockFor > 0 {
		select {
		case <-time.After(s.blockFor):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if atomic.AddInt32(&s.failN, -1) >= 0 {
		return s.failWith
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]Entry, len(entries))
	copy(cp, entries)
	s.appends = append(s.appends, appendCall{key: key, entries: cp})
	return nil
}

func (s *recordingStore) Load(context.Context, SessionKey) ([]Entry, error) { return nil, nil }

func (s *recordingStore) calls() []appendCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]appendCall(nil), s.appends...)
}

const testProjectsDir = "/root/.claude/projects"

func mainTranscriptPath(pk, sid string) string {
	return filepath.Join(testProjectsDir, pk, sid+".jsonl")
}

func TestBatcherFlushOnDemand(t *testing.T) {
	store := &recordingStore{}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeBatched)
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	// Nothing flushed yet (no result, under thresholds).
	if len(store.calls()) != 0 {
		t.Fatalf("expected 0 appends before flush, got %d", len(store.calls()))
	}
	b.Flush(context.Background())
	calls := store.calls()
	if len(calls) != 1 || calls[0].key.SessionID != "s1" {
		t.Fatalf("expected 1 append for s1, got %+v", calls)
	}
}

func TestBatcherCoalescesByFilePath(t *testing.T) {
	store := &recordingStore{}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeBatched)
	p := mainTranscriptPath("proj", "s1")
	b.Enqueue(p, []Entry{Entry(`{"uuid":"a"}`)})
	b.Enqueue(p, []Entry{Entry(`{"uuid":"b"}`)})
	b.Enqueue(mainTranscriptPath("proj", "s2"), []Entry{Entry(`{"uuid":"c"}`)})
	b.Flush(context.Background())
	calls := store.calls()
	// s1 coalesced into one append of 2 entries; s2 one append of 1.
	var s1, s2 *appendCall
	for i := range calls {
		switch calls[i].key.SessionID {
		case "s1":
			s1 = &calls[i]
		case "s2":
			s2 = &calls[i]
		}
	}
	if s1 == nil || len(s1.entries) != 2 {
		t.Fatalf("expected s1 append of 2 entries, got %+v", s1)
	}
	if s2 == nil || len(s2.entries) != 1 {
		t.Fatalf("expected s2 append of 1 entry, got %+v", s2)
	}
}

func TestBatcherEagerFlushOnThreshold(t *testing.T) {
	store := &recordingStore{}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeEager)
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	// Eager mode schedules a background flush; wait for it via Close.
	b.Close()
	if len(store.calls()) != 1 {
		t.Fatalf("expected 1 append after eager flush, got %d", len(store.calls()))
	}
}

func TestBatcherRetriesThenSucceeds(t *testing.T) {
	store := &recordingStore{failN: 2, failWith: errors.New("transient")}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeBatched)
	// Shrink backoff so the test is fast.
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	b.Flush(context.Background())
	if len(store.calls()) != 1 {
		t.Fatalf("expected append to succeed on 3rd attempt, got %d appends", len(store.calls()))
	}
}

func TestBatcherReportsMirrorErrorAfterExhaustingRetries(t *testing.T) {
	store := &recordingStore{failN: 100, failWith: errors.New("boom")}
	var gotKey SessionKey
	var gotMsg string
	var mu sync.Mutex
	called := 0
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, func(key SessionKey, msg string) {
		mu.Lock()
		defer mu.Unlock()
		called++
		gotKey = key
		gotMsg = msg
	}, FlushModeBatched)
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	b.Flush(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if called != 1 {
		t.Fatalf("expected onError called once, got %d", called)
	}
	if gotKey.SessionID != "s1" {
		t.Fatalf("onError key = %+v, want s1", gotKey)
	}
	if gotMsg == "" {
		t.Fatal("onError msg empty")
	}
}

func TestBatcherTimeoutNotRetried(t *testing.T) {
	// blockFor > sendTimeout forces a deadline; the call must not be retried.
	store := &recordingStore{blockFor: 200 * time.Millisecond}
	var attempts int32
	countingStore := &attemptCounter{recordingStore: store, attempts: &attempts}
	b := NewTranscriptMirrorBatcher(countingStore, testProjectsDir, func(SessionKey, string) {}, FlushModeBatched)
	b.sendTimeout = 20 * time.Millisecond
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	b.Flush(context.Background())
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Fatalf("expected exactly 1 attempt on timeout (no retry), got %d", n)
	}
}

type attemptCounter struct {
	*recordingStore
	attempts *int32
}

func (a *attemptCounter) Append(ctx context.Context, key SessionKey, entries []Entry) error {
	atomic.AddInt32(a.attempts, 1)
	return a.recordingStore.Append(ctx, key, entries)
}

func TestBatcherCloseFlushesLastBatch(t *testing.T) {
	store := &recordingStore{}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeBatched)
	b.Enqueue(mainTranscriptPath("proj", "s1"), []Entry{Entry(`{"uuid":"a"}`)})
	b.Close()
	if len(store.calls()) != 1 {
		t.Fatalf("expected Close to flush last batch, got %d appends", len(store.calls()))
	}
	// Enqueue after Close is dropped.
	b.Enqueue(mainTranscriptPath("proj", "s2"), []Entry{Entry(`{"uuid":"b"}`)})
	if len(store.calls()) != 1 {
		t.Fatalf("expected enqueue-after-close to be dropped, got %d appends", len(store.calls()))
	}
}

func TestBatcherDropsFrameOutsideProjectsDir(t *testing.T) {
	store := &recordingStore{}
	var errCalled int32
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, func(SessionKey, string) {
		atomic.AddInt32(&errCalled, 1)
	}, FlushModeBatched)
	b.Enqueue("/somewhere/else/x.jsonl", []Entry{Entry(`{"uuid":"a"}`)})
	b.Flush(context.Background())
	if len(store.calls()) != 0 {
		t.Fatalf("expected frame outside projects dir to be dropped, got %d appends", len(store.calls()))
	}
	if atomic.LoadInt32(&errCalled) != 0 {
		t.Fatal("dropping an out-of-dir frame should not report a mirror error")
	}
}

// TestBatcherConcurrentFlushNoLossNoDup exercises many concurrent drains
// (eager mode spawns a background flush per enqueue) plus interleaved explicit
// flushes and a Close, asserting the store receives every entry exactly once
// with no loss, no duplication, and no data race (run with -race).
//
// Note: this is a concurrency-safety guard, not a differential test for the
// detach-under-flushMu ordering fix. The reorder window in the old code
// (detach, then race for flushMu) is a back-to-back pair with no blocking point
// between, so it cannot be forced deterministically from a black-box test. The
// fix is structural: drain now acquires flushMu BEFORE detaching, making each
// drainer's detach+flush atomic so append order is preserved by construction.
func TestBatcherConcurrentFlushNoLossNoDup(t *testing.T) {
	store := &orderingStore{}
	b := NewTranscriptMirrorBatcher(store, testProjectsDir, nil, FlushModeEager)
	p := mainTranscriptPath("proj", "s1")

	const n = 300
	for i := 0; i < n; i++ {
		b.Enqueue(p, []Entry{Entry(fmt.Sprintf(`{"uuid":"u%04d","seq":%d}`, i, i))})
		if i%11 == 0 {
			b.Flush(context.Background())
		}
	}
	b.Close()

	got := store.seqs()
	if len(got) != n {
		t.Fatalf("expected %d entries, got %d (loss or duplication)", n, len(got))
	}
	seen := make(map[int]bool, n)
	for _, seq := range got {
		if seq < 0 || seq >= n {
			t.Fatalf("out-of-range seq %d", seq)
		}
		if seen[seq] {
			t.Fatalf("duplicate seq %d", seq)
		}
		seen[seq] = true
	}
}

// orderingStore records the "seq" field of every appended entry in append order.
type orderingStore struct {
	mu   sync.Mutex
	seen []int
}

func (s *orderingStore) Append(_ context.Context, _ SessionKey, entries []Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		var m struct {
			Seq int `json:"seq"`
		}
		_ = jsonUnmarshal(e, &m)
		s.seen = append(s.seen, m.Seq)
	}
	return nil
}

func (s *orderingStore) Load(context.Context, SessionKey) ([]Entry, error) { return nil, nil }

func (s *orderingStore) seqs() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.seen...)
}

func jsonUnmarshal(e Entry, v any) error { return json.Unmarshal(e, v) }
