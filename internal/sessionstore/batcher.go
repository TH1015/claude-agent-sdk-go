package sessionstore

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Eager-flush thresholds and retry policy. Exported for tests.
const (
	// MaxPendingEntries triggers a background flush once the pending buffer
	// holds more than this many entries.
	MaxPendingEntries = 500
	// MaxPendingBytes triggers a background flush once the pending buffer's
	// approximate wire size exceeds this many bytes (1 MiB).
	MaxPendingBytes = 1 << 20
	// SendTimeout bounds a single store.Append call.
	SendTimeout = 60 * time.Second
	// mirrorAppendMaxAttempts is the total number of append attempts per batch.
	mirrorAppendMaxAttempts = 3
)

// mirrorAppendBackoff holds the delays between attempts. Its length must be
// mirrorAppendMaxAttempts-1 (one delay between each pair of attempts).
var mirrorAppendBackoff = []time.Duration{200 * time.Millisecond, 800 * time.Millisecond}

// MirrorErrorFunc reports a permanently-failed mirror append. key may be a zero
// SessionKey when the frame could not be mapped to a key.
type MirrorErrorFunc func(key SessionKey, msg string)

// mirrorEntry is one buffered frame awaiting flush.
type mirrorEntry struct {
	filePath string
	entries  []Entry
	bytes    int
}

// TranscriptMirrorBatcher accumulates transcript_mirror frames and flushes them
// to a Store. Enqueue is fire-and-forget; Flush and Close are synchronous.
//
// The pending queue is bounded — when it exceeds maxPendingEntries or
// maxPendingBytes an eager flush fires in the background so memory stays flat
// during long turns where no result (and thus no explicit Flush) arrives.
//
// Adapter failures are retried (mirrorAppendMaxAttempts total) with short
// backoff; timeouts are NOT retried since the in-flight call may still land.
// Only after the final attempt fails is the batch dropped and reported via
// onError. Failures never propagate out of Enqueue/Flush — the local-disk
// transcript is already durable so the session must continue unaffected.
// Adapters should dedupe by entry "uuid" since a retried batch may partially
// overlap a prior partial write.
//
// Concurrency model (mirrors the Python anyio.Lock + detached task design):
//   - flushMu serializes drains so append ordering holds.
//   - a WaitGroup tracks in-flight background flush goroutines so Close can
//     wait for them.
//   - the final Close flush uses context.Background() (not a possibly-cancelled
//     caller context) so the last batch still reaches the store on disconnect.
type TranscriptMirrorBatcher struct {
	store       Store
	projectsDir string
	onError     MirrorErrorFunc
	sendTimeout time.Duration

	maxPendingEntries int
	maxPendingBytes   int

	mu           sync.Mutex // guards pending buffer + counters + closed
	pending      []mirrorEntry
	pendingCount int
	pendingBytes int
	closed       bool

	flushMu sync.Mutex     // serializes drains (append ordering)
	wg      sync.WaitGroup // tracks background flush goroutines
}

// NewTranscriptMirrorBatcher constructs a batcher. When flushMode is
// FlushModeEager the pending thresholds are zeroed so every enqueued frame
// schedules a background flush; otherwise the defaults apply.
func NewTranscriptMirrorBatcher(store Store, projectsDir string, onError MirrorErrorFunc, flushMode FlushMode) *TranscriptMirrorBatcher {
	b := &TranscriptMirrorBatcher{
		store:             store,
		projectsDir:       projectsDir,
		onError:           onError,
		sendTimeout:       SendTimeout,
		maxPendingEntries: MaxPendingEntries,
		maxPendingBytes:   MaxPendingBytes,
	}
	if flushMode == FlushModeEager {
		b.maxPendingEntries = 0
		b.maxPendingBytes = 0
	}
	return b
}

// Enqueue buffers a frame and schedules an eager background flush if the
// pending buffer exceeds a size threshold. Fire-and-forget.
func (b *TranscriptMirrorBatcher) Enqueue(filePath string, entries []Entry) {
	if len(entries) == 0 {
		return
	}
	// Approximate wire size — one marshal of the batch (not per entry).
	size := approxSize(entries)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.pending = append(b.pending, mirrorEntry{filePath: filePath, entries: entries, bytes: size})
	b.pendingCount += len(entries)
	b.pendingBytes += size
	overflow := b.pendingCount > b.maxPendingEntries || b.pendingBytes > b.maxPendingBytes
	b.mu.Unlock()

	if overflow {
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.drain(context.Background())
		}()
	}
}

// Flush flushes all pending entries, serialized after any in-flight eager flush.
func (b *TranscriptMirrorBatcher) Flush(ctx context.Context) {
	b.drain(ctx)
}

// Close performs a final flush before teardown and waits for any in-flight
// background flushes. Uses context.Background() so the last batch still reaches
// the store even if the caller's context was cancelled. Never panics.
func (b *TranscriptMirrorBatcher) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	// Drain whatever is pending on the current goroutine.
	b.drain(context.Background())
	// Wait for any background flushes still running.
	b.wg.Wait()
}

// drain sends the pending buffer to the store, coalesced by file path, in
// append order. Never panics; adapter and onError errors are contained.
//
// flushMu is acquired BEFORE detaching the pending buffer so concurrent drains
// (eager/threshold-triggered background flushes plus an explicit Flush) can
// never reorder a transcript: whichever goroutine wins flushMu also detaches
// and writes the earliest-accumulated batch first. Enqueue only takes the
// separate b.mu, so it keeps accumulating into a fresh buffer while a flush is
// in flight — the next drainer picks that up, still in order.
func (b *TranscriptMirrorBatcher) drain(ctx context.Context) {
	b.flushMu.Lock()

	b.mu.Lock()
	items := b.pending
	b.pending = nil
	b.pendingCount = 0
	b.pendingBytes = 0
	b.mu.Unlock()

	if len(items) == 0 {
		b.flushMu.Unlock()
		return
	}

	var errs []mirrorFailure
	b.doFlush(ctx, items, &errs)
	b.flushMu.Unlock()

	// Report errors after releasing the lock so a slow onError callback can't
	// block subsequent drains.
	if b.onError != nil {
		for _, e := range errs {
			b.reportError(e.key, e.msg)
		}
	}
}

type mirrorFailure struct {
	key SessionKey
	msg string
}

// reportError invokes onError, recovering from any panic in the user callback.
func (b *TranscriptMirrorBatcher) reportError(key SessionKey, msg string) {
	defer func() { _ = recover() }()
	b.onError(key, msg)
}

// doFlush coalesces items by filePath (one append per unique file), maps each
// to a SessionKey, and sends with bounded retry.
func (b *TranscriptMirrorBatcher) doFlush(ctx context.Context, items []mirrorEntry, errs *[]mirrorFailure) {
	// Preserve first-seen file order; entries within a path keep enqueue order.
	order := make([]string, 0, len(items))
	byPath := make(map[string][]Entry)
	for _, item := range items {
		if _, seen := byPath[item.filePath]; !seen {
			order = append(order, item.filePath)
		}
		byPath[item.filePath] = append(byPath[item.filePath], item.entries...)
	}

	for _, filePath := range order {
		entries := byPath[filePath]
		if len(entries) == 0 {
			continue
		}
		key, ok := FilePathToSessionKey(filePath, b.projectsDir)
		if !ok {
			// filePath is not under projectsDir — subprocess CLAUDE_CONFIG_DIR
			// likely differs from the parent. Drop the frame.
			continue
		}
		if err := b.sendWithRetry(ctx, key, entries); err != nil {
			*errs = append(*errs, mirrorFailure{key: key, msg: err.Error()})
		}
	}
}

// sendWithRetry attempts store.Append up to mirrorAppendMaxAttempts times with
// backoff. Timeouts are not retried. Returns the last error on permanent
// failure, or nil on success.
func (b *TranscriptMirrorBatcher) sendWithRetry(ctx context.Context, key SessionKey, entries []Entry) error {
	var lastErr error
	for attempt := 0; attempt < mirrorAppendMaxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(mirrorAppendBackoff[attempt-1]):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, b.sendTimeout)
		err := b.store.Append(callCtx, key, entries)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		// Don't retry on timeout: the in-flight call may still land, so a
		// retry would launch a concurrent duplicate.
		if callCtx.Err() == context.DeadlineExceeded {
			return lastErr
		}
	}
	return lastErr
}

// approxSize estimates the wire size of a batch with a single marshal.
func approxSize(entries []Entry) int {
	buf, err := json.Marshal(entries)
	if err != nil {
		// Fall back to a raw byte-length sum.
		total := 0
		for _, e := range entries {
			total += len(e)
		}
		return total
	}
	return len(buf)
}
