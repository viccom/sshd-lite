//go:build webssh

package webssh

import (
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Terminal wraps one SSH session running an interactive shell with a PTY.
type Terminal struct {
	sess  *ssh.Session
	stdin io.WriteCloser
	done  chan error
	once  sync.Once
}

// NewTerminal starts a PTY shell on client, mirroring both stdout and
// stderr into out. cols/rows fall back to 80x24 when non positive.
func NewTerminal(client *ssh.Client, out io.Writer, cols, rows int) (*Terminal, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	sess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("webssh: new session: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, fmt.Errorf("webssh: stdin pipe: %w", err)
	}
	sess.Stdout = out
	// Stderr is deliberately left nil: the remote PTY merges stderr into the
	// terminal stream, and wiring Stderr to the same writer would make the
	// ssh library run two concurrent copy goroutines on it — a data race
	// unless the caller's writer is mutex-protected.
	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("webssh: request pty: %w", err)
	}
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("webssh: start shell: %w", err)
	}
	t := &Terminal{sess: sess, stdin: stdin, done: make(chan error, 1)}
	go func() { t.done <- sess.Wait() }()
	return t, nil
}

// Write feeds terminal stdin (browser keystrokes).
func (t *Terminal) Write(p []byte) (int, error) { return t.stdin.Write(p) }

// Resize updates the PTY size.
func (t *Terminal) Resize(cols, rows int) error {
	return t.sess.WindowChange(rows, cols)
}

// Done reports shell exit; exactly one value is delivered.
func (t *Terminal) Done() <-chan error { return t.done }

// Close tears down the session (the sshd-lite shell reaper handles the
// process: Interrupt, then Kill after its bounded grace period).
func (t *Terminal) Close() error {
	t.once.Do(func() { t.sess.Close() })
	return nil
}
