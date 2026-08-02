package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// DefaultRefreshCommand is the executable used by gateway entrypoints when
	// no external credential refresh command is configured.
	DefaultRefreshCommand = "agw-auth"
	// DefaultRefreshCommandArg is the default static argument paired with
	// DefaultRefreshCommand by gateway entrypoints.
	DefaultRefreshCommandArg     = "refresh"
	defaultCommandRefreshTimeout = 30 * time.Second
	maxCommandRefreshStdout      = 64 << 10
	maxCommandRefreshStderr      = 16 << 10
	commandRefreshWaitDelay      = time.Second
)

// CommandRefresher implements the generic external refresh protocol. It sends
// one credential as JSON on stdin and expects the refreshed credential as JSON
// on stdout. Provider-specific OAuth behavior remains outside the gateway.
type CommandRefresher struct {
	Command string
	Args    []string
	timeout time.Duration
}

func NewCommandRefresher(command string, args ...string) *CommandRefresher {
	return &CommandRefresher{
		Command: strings.TrimSpace(command),
		Args:    append([]string(nil), args...),
		timeout: defaultCommandRefreshTimeout,
	}
}

func (r *CommandRefresher) Refresh(ctx context.Context, cred *Credential) (*Credential, error) {
	if r == nil || r.Command == "" {
		return nil, fmt.Errorf("credential refresh command is empty")
	}
	payload, err := json.Marshal(cred)
	if err != nil {
		return nil, fmt.Errorf("encode credential for refresh: %w", err)
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultCommandRefreshTimeout
	}
	refreshCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(refreshCtx, r.Command, r.Args...)
	cmd.WaitDelay = commandRefreshWaitDelay
	cmd.Stdin = bytes.NewReader(payload)
	stdout := newBoundedBuffer(maxCommandRefreshStdout)
	stderr := newBoundedBuffer(maxCommandRefreshStderr)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if stderr.Truncated() {
			detail += " (stderr truncated)"
		}
		if ctxErr := refreshCtx.Err(); ctxErr != nil {
			if detail == "" {
				detail = ctxErr.Error()
			} else {
				detail = ctxErr.Error() + ": " + detail
			}
		} else if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("run credential refresher %q: %s", r.Command, detail)
	}
	if stdout.Truncated() {
		return nil, fmt.Errorf("credential refresher output exceeds %d bytes", maxCommandRefreshStdout)
	}

	var updated Credential
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		return nil, fmt.Errorf("decode credential refresher output: %w", err)
	}
	return &updated, nil
}

// boundedBuffer retains only the first limit bytes while reporting successful
// writes to os/exec, allowing a noisy child to finish without growing memory.
type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) boundedBuffer {
	return boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = b.buffer.Write(p[:remaining])
	}
	if remaining < len(p) {
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) Truncated() bool {
	return b != nil && b.truncated
}

func (b *boundedBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buffer.Bytes()
}

func (b *boundedBuffer) String() string {
	if b == nil {
		return ""
	}
	return b.buffer.String()
}
