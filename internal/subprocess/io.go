package subprocess

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/TH1015/claude-agent-sdk-go/internal/parser"
	"github.com/TH1015/claude-agent-sdk-go/internal/sessionstore"
	"github.com/TH1015/claude-agent-sdk-go/internal/shared"
)

// handleStdout processes stdout in a separate goroutine
func (t *Transport) handleStdout() {
	defer t.wg.Done()
	defer close(t.msgChan)
	defer close(t.errChan)
	defer t.validator.MarkStreamEnd() // Mark stream end for validation

	scanner := bufio.NewScanner(t.stdout)

	// Scanner token size must match the parser's buffer limit so lines aren't
	// truncated before parsing. Default is 64KB; respect MaxBufferSize if set.
	scanTokenSize := parser.MaxBufferSize
	if t.options != nil && t.options.MaxBufferSize != nil {
		scanTokenSize = *t.options.MaxBufferSize
	}
	buf := make([]byte, scanTokenSize)
	scanner.Buffer(buf, scanTokenSize)

	for scanner.Scan() {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		line := scanner.Text()
		if line == "" {
			continue
		}

		// Parse line with the parser
		messages, err := t.parser.ProcessLine(line)
		if err != nil {
			select {
			case t.errChan <- err:
			case <-t.ctx.Done():
				return
			}
			continue
		}

		// Send parsed messages and track for validation
		for _, msg := range messages {
			if msg == nil {
				continue
			}

			// If this is an error ResultMessage before we're fully connected,
			// it means the CLI failed during init (e.g., invalid session ID).
			// Route the error to the control protocol to unblock Initialize().
			t.routeInitError(msg)

			// Check if this is a control message that should be routed to the protocol
			if rawCtrl, ok := msg.(*shared.RawControlMessage); ok {
				// Route control messages to the protocol for request/response correlation
				if t.protocol != nil {
					// HandleIncomingMessage routes control responses to pending requests
					// and forwards non-control messages to the protocol's message stream
					_ = t.protocol.HandleIncomingMessage(t.ctx, rawCtrl.Data)
				}
				// Don't send control messages to msgChan - they're internal to the protocol
				continue
			}

			// Intercept transcript_mirror frames (forwarded to the
			// SessionStore batcher, never delivered) and flush the batcher on
			// result messages. Returns true when the message was consumed here.
			if t.handleMirrorMessage(msg) {
				continue
			}

			// Track regular message for stream validation
			t.validator.TrackMessage(msg)

			select {
			case t.msgChan <- msg:
			case <-t.ctx.Done():
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		select {
		case t.errChan <- fmt.Errorf("stdout scanner error: %w", err):
		case <-t.ctx.Done():
		}
	}
}

// handleMirrorMessage intercepts SessionStore transcript_mirror frames and
// flushes the mirror batcher on result messages. Returns true when the message
// was a transcript_mirror frame (consumed here and not delivered to consumers);
// result messages return false so they still reach the consumer after flushing.
func (t *Transport) handleMirrorMessage(msg shared.Message) bool {
	if mirror, ok := msg.(*shared.TranscriptMirrorMessage); ok {
		if t.mirrorBatcher != nil {
			entries := make([]sessionstore.Entry, len(mirror.Entries))
			for i, e := range mirror.Entries {
				entries[i] = sessionstore.Entry(e)
			}
			t.mirrorBatcher.Enqueue(mirror.FilePath, entries)
		}
		return true
	}
	// On a result message, explicitly flush the mirror batcher so the turn's
	// transcript reaches the store before the consumer observes completion.
	// Best-effort; failures surface as mirror_error messages via onError.
	if t.mirrorBatcher != nil {
		if _, ok := msg.(*shared.ResultMessage); ok {
			t.mirrorBatcher.Flush(t.ctx)
		}
	}
	return false
}

// handleStderrCallback processes stderr in a separate goroutine.
// Reads line-by-line, strips trailing whitespace, skips empty lines, and
// silently ignores scanner errors.
func (t *Transport) handleStderrCallback() {
	defer t.wg.Done()

	scanner := bufio.NewScanner(t.stderrPipe)

	for scanner.Scan() {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		// Strip trailing whitespace (matches Python's rstrip())
		line := strings.TrimRight(scanner.Text(), " \t\r\n")

		// Skip empty lines (matches Python SDK behavior)
		if line == "" {
			continue
		}

		// Call the callback synchronously (matches Python SDK)
		// Recover from panics to prevent crashing the SDK
		func() {
			defer func() {
				_ = recover() // Silently ignore callback panics (matches Python's pass)
			}()
			t.options.StderrCallback(line)
		}()
	}
	// Silently ignore scanner errors (matches Python SDK's except Exception: pass)
}

// routeInitError checks if a message is an error ResultMessage arriving before
// the transport is fully connected, and routes it to the control protocol to
// unblock Initialize().
func (t *Transport) routeInitError(msg shared.Message) {
	resultMsg, ok := msg.(*shared.ResultMessage)
	if !ok || t.connected || !resultMsg.IsError || t.protocol == nil {
		return
	}
	t.protocol.HandleControlInitErr(errors.New(formatInitError(resultMsg)))
}

// formatInitError builds a meaningful error string from a ResultMessage that
// arrived during initialization. Prefers Errors, falls back to Result, then Subtype.
func formatInitError(msg *shared.ResultMessage) string {
	if len(msg.Errors) > 0 {
		return strings.Join(msg.Errors, "; ")
	}
	if msg.Result != nil && *msg.Result != "" {
		return *msg.Result
	}
	return fmt.Sprintf("initialization failed with subtype: %s", msg.Subtype)
}

// setupStderr configures stderr handling based on options.
// Precedence: StderrCallback > DebugWriter > temp file (default).
func (t *Transport) setupStderr() error {
	switch {
	case t.options != nil && t.options.StderrCallback != nil:
		// Create pipe for callback-based stderr handling
		stderrPipe, err := t.cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("failed to create stderr pipe: %w", err)
		}
		t.stderrPipe = stderrPipe
	case t.options != nil && t.options.DebugWriter != nil:
		// Use custom debug writer provided by user
		t.cmd.Stderr = t.options.DebugWriter
	default:
		// Isolate stderr using temporary file to prevent deadlocks
		// This matches Python SDK pattern to avoid subprocess pipe deadlocks
		stderrFile, err := os.CreateTemp("", "claude_stderr_*.log")
		if err != nil {
			return fmt.Errorf("failed to create stderr file: %w", err)
		}
		t.stderr = stderrFile
		t.cmd.Stderr = t.stderr
	}
	return nil
}

// setupIoPipes configures stdin, stdout, and stderr pipes for the subprocess.
// For streaming mode, creates a stdin pipe for sending messages. Always creates
// stdout pipe for receiving responses. Stderr is configured via setupStderr.
func (t *Transport) setupIoPipes() error {
	var err error
	if t.promptArg == nil {
		// Only create stdin pipe if we need to send messages via stdin
		t.stdin, err = t.cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdin pipe: %w", err)
		}
	}

	t.stdout, err = t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Handle stderr configuration
	if err := t.setupStderr(); err != nil {
		return err
	}

	return nil
}
