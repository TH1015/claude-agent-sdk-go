package claudecode

import (
	"context"
	"time"

	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
)

// prepareSessionStore validates SessionStore option combinations and, when a
// store is paired with resume/continue, materializes the session from the store
// into a temporary CLAUDE_CONFIG_DIR so the CLI subprocess can resume it. It
// mutates options in place (sets ExtraEnv[CLAUDE_CONFIG_DIR], Resume, and
// clears ContinueConversation) and returns a cleanup func that removes the temp
// dir. The cleanup is always non-nil (a no-op when nothing was materialized).
//
// Called before transport creation so the transport's auto-built mirror
// batcher resolves the (possibly temp) projects dir from ExtraEnv. Mirrors the
// Python SDK's client.py/query.py ordering.
func prepareSessionStore(ctx context.Context, options *Options) (cleanup func(), err error) {
	cleanup = func() {}
	if options == nil || options.SessionStore == nil {
		return cleanup, nil
	}
	store, ok := options.SessionStore.(sessionstore.Store)
	if !ok {
		return cleanup, nil
	}

	hasResume := options.Resume != nil && *options.Resume != ""
	if verr := sessionstore.ValidateOptions(store, options.ContinueConversation, hasResume, options.EnableFileCheckpointing); verr != nil {
		return cleanup, verr
	}

	resume := ""
	if options.Resume != nil {
		resume = *options.Resume
	}
	cwd := ""
	if options.Cwd != nil {
		cwd = *options.Cwd
	}
	loadTimeout := time.Duration(options.LoadTimeoutMS) * time.Millisecond

	materialized, merr := sessionstore.MaterializeResumeSession(
		ctx, store, resume, options.ContinueConversation, cwd, options.ExtraEnv, loadTimeout,
	)
	if merr != nil {
		return cleanup, merr
	}
	if materialized == nil {
		return cleanup, nil
	}

	// Repoint the subprocess at the temp config dir, resolve resume to the
	// concrete session id, and clear continue (already resolved).
	if options.ExtraEnv == nil {
		options.ExtraEnv = map[string]string{}
	}
	options.ExtraEnv["CLAUDE_CONFIG_DIR"] = materialized.ConfigDir
	options.Resume = &materialized.ResumeSessionID
	options.ContinueConversation = false

	return func() { materialized.Cleanup() }, nil
}
