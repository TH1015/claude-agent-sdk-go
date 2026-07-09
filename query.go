package claudecode

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/TH1015/claude-agent-sdk-go/internal/cli"
	"github.com/TH1015/claude-agent-sdk-go/internal/subprocess"
)

// ErrNoMoreMessages indicates the message iterator has no more messages.
var ErrNoMoreMessages = errors.New("no more messages")

// Query executes a one-shot query with automatic cleanup.
// This follows the Python SDK pattern but uses dependency injection for transport.
func Query(ctx context.Context, prompt string, opts ...Option) (MessageIterator, error) {
	options := NewOptions(opts...)

	// If a SessionStore is configured with resume/continue, materialize the
	// session into a temp CLAUDE_CONFIG_DIR before building the transport so the
	// CLI can resume it. Also validates store option combinations (fail-fast).
	cleanup, err := prepareSessionStore(ctx, options)
	if err != nil {
		return nil, err
	}

	// For one-shot queries, create a transport that passes prompt as CLI argument
	// This matches the Python SDK behavior where prompt is passed via --print flag
	transport, err := createQueryTransport(prompt, options)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to create query transport: %w", err)
	}

	iter, err := queryWithTransportAndOptions(ctx, prompt, transport, options)
	if err != nil {
		cleanup()
		return nil, err
	}
	if qi, ok := iter.(*queryIterator); ok {
		qi.cleanup = cleanup
	}
	return iter, nil
}

// QueryWithTransport executes a query with a custom transport.
// The transport parameter is required and must not be nil.
func QueryWithTransport(
	ctx context.Context,
	prompt string,
	transport Transport,
	opts ...Option,
) (MessageIterator, error) {
	if transport == nil {
		return nil, fmt.Errorf("transport is required")
	}

	options := NewOptions(opts...)
	return queryWithTransportAndOptions(ctx, prompt, transport, options)
}

// Internal helper functions
func queryWithTransportAndOptions(
	ctx context.Context,
	prompt string,
	transport Transport,
	options *Options,
) (MessageIterator, error) {
	if transport == nil {
		return nil, fmt.Errorf("transport is required")
	}

	// Create iterator that manages the transport lifecycle
	return &queryIterator{
		transport: transport,
		prompt:    prompt,
		ctx:       ctx,
		options:   options,
	}, nil
}

// queryIterator implements MessageIterator for simple queries
type queryIterator struct {
	transport Transport
	prompt    string
	ctx       context.Context
	options   *Options
	started   bool
	msgChan   <-chan Message
	errChan   <-chan error
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	// cleanup removes any temp resume-materialization dir; nil-safe via Close.
	cleanup func()
}

func (qi *queryIterator) Next(_ context.Context) (Message, error) {
	qi.mu.Lock()
	if qi.closed {
		qi.mu.Unlock()
		return nil, ErrNoMoreMessages
	}

	// Initialize on first call
	if !qi.started {
		if err := qi.start(); err != nil {
			qi.mu.Unlock()
			return nil, err
		}
		qi.started = true
	}
	qi.mu.Unlock()

	// Read from message channels
	select {
	case msg, ok := <-qi.msgChan:
		if !ok {
			qi.mu.Lock()
			qi.closed = true
			qi.mu.Unlock()
			return nil, ErrNoMoreMessages
		}
		return msg, nil
	case err := <-qi.errChan:
		qi.mu.Lock()
		qi.closed = true
		qi.mu.Unlock()
		return nil, err
	case <-qi.ctx.Done():
		qi.mu.Lock()
		qi.closed = true
		qi.mu.Unlock()
		return nil, qi.ctx.Err()
	}
}

func (qi *queryIterator) Close() error {
	var err error
	qi.closeOnce.Do(func() {
		qi.mu.Lock()
		qi.closed = true
		qi.mu.Unlock()
		if qi.transport != nil {
			err = qi.transport.Close()
		}
		if qi.cleanup != nil {
			qi.cleanup()
		}
	})
	return err
}

func (qi *queryIterator) start() error {
	// Connect to transport
	if err := qi.transport.Connect(qi.ctx); err != nil {
		return fmt.Errorf("failed to connect transport: %w", err)
	}

	// Get message channels
	msgChan, errChan := qi.transport.ReceiveMessages(qi.ctx)
	qi.msgChan = msgChan
	qi.errChan = errChan

	// Send the prompt
	userMsg := &UserMessage{Content: qi.prompt}
	streamMsg := StreamMessage{
		Type:    "request",
		Message: userMsg,
	}

	if err := qi.transport.SendMessage(qi.ctx, streamMsg); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	return nil
}

// createQueryTransport creates a transport for one-shot queries with prompt as CLI argument.
//
// If the caller supplied a CLI path via WithCLIPath, that path is used directly
// and CLI auto-discovery is skipped. This matches the documented behaviour of
// the option (and the Python SDK's `cli_path` parameter) and lets callers
// bypass exec.LookPath when, for example, the npm shim on their platform
// mishandles command-line arguments.
func createQueryTransport(prompt string, options *Options) (Transport, error) {
	cliPath, err := resolveCLIPath(options)
	if err != nil {
		return nil, err
	}

	// Create subprocess transport with prompt as CLI argument
	return subprocess.NewWithPrompt(cliPath, options, prompt), nil
}

// resolveCLIPath returns the CLI path the transport should invoke. When
// options.CLIPath is set and non-empty, it wins over auto-discovery — the
// caller has explicitly opted out of FindCLI's PATH/well-known-location
// search.
func resolveCLIPath(options *Options) (string, error) {
	if options != nil && options.CLIPath != nil && *options.CLIPath != "" {
		return *options.CLIPath, nil
	}
	return cli.FindCLI()
}
