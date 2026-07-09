package subprocess

import (
	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
	"github.com/TH1015/claude-agent-sdk-go/internal/shared"
)

// setupMirrorBatcher constructs a TranscriptMirrorBatcher from options when a
// SessionStore is configured, unless one was already injected via
// SetMirrorBatcher (e.g. by the resume-materialization wiring, which knows the
// temp projects dir). Must be called after the msgChan is initialized (so the
// onError callback can inject mirror_error messages) and before the stdout
// goroutine starts.
func (t *Transport) setupMirrorBatcher() {
	if t.mirrorBatcher != nil {
		return // already set (e.g. resume path supplied the temp projects dir)
	}
	if t.options == nil || t.options.SessionStore == nil {
		return
	}
	store, ok := t.options.SessionStore.(sessionstore.Store)
	if !ok {
		return
	}

	projectsDir, err := sessionstore.ProjectsDirForEnv(t.options.ExtraEnv)
	if err != nil {
		// Without a projects dir the batcher can't map file paths to keys;
		// skip mirroring rather than fail the whole session.
		return
	}

	flushMode := sessionstore.FlushModeBatched
	if fm, ok := t.options.SessionStoreFlush.(sessionstore.FlushMode); ok && fm != "" {
		flushMode = fm
	}

	t.mirrorBatcher = sessionstore.NewTranscriptMirrorBatcher(
		store, projectsDir, t.mirrorOnError, flushMode,
	)
}

// mirrorOnError injects a mirror_error system message into the consumer stream
// so callers can observe permanent store-append failures. Best-effort: drops
// the message if the channel is full or the context is done.
func (t *Transport) mirrorOnError(key sessionstore.SessionKey, msg string) {
	if t.msgChan == nil {
		return
	}
	errMsg := &shared.MirrorErrorMessage{
		ProjectKey: key.ProjectKey,
		SessionID:  key.SessionID,
		Subpath:    key.Subpath,
		Err:        msg,
	}
	select {
	case t.msgChan <- errMsg:
	case <-t.ctx.Done():
	}
}
