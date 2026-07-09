package sessionstore

import "errors"

// ValidateOptions checks SessionStore option combinations, returning an error
// for invalid ones. Called before subprocess spawn so misconfiguration fails
// fast. Mirrors the Python SDK's validate_session_store_options.
//
// storeConfigured reports whether a SessionStore is set. The two illegal
// combinations are:
//   - a store with file checkpointing (checkpoint blobs are local-disk only and
//     would diverge from the mirrored transcript);
//   - continue-conversation without an explicit resume when the store cannot
//     enumerate sessions (ListSessions is required to resolve the latest one).
func ValidateOptions(store Store, continueConversation bool, hasResume, enableFileCheckpointing bool) error {
	if store == nil {
		return nil
	}
	if enableFileCheckpointing {
		return errors.New("session_store cannot be combined with enable_file_checkpointing " +
			"(checkpoints are local-disk only and would diverge from the mirrored transcript)")
	}
	if continueConversation && !hasResume {
		if _, ok := AsSessionLister(store); !ok {
			return errors.New("continue_conversation with session_store requires the store to implement ListSessions()")
		}
	}
	return nil
}
