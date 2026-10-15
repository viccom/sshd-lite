//go:build webssh

package webssh

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jpillora/sshd-lite/sshd"
)

func newTestBridge(t *testing.T) *Bridge {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("interactive PTY tests run under WSL (bash); Windows host skips")
	}
	br, err := NewBridge(sshd.Config{WorkDir: t.TempDir(), Shell: os.Getenv("TEST_SHELL"), LogQuiet: true})
	if err != nil {
		t.Fatal(err)
	}
	return br
}

func TestTerminalEcho(t *testing.T) {
	br := newTestBridge(t)
	cl, err := br.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	var out bytes.Buffer
	term, err := NewTerminal(cl, &out, 80, 24)
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	defer term.Close()

	if _, err := term.Write([]byte("echo term-ok\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "term-ok") {
			return // pass
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no echo within 15s, output so far: %q", out.String())
}

func TestTerminalResizeAndClose(t *testing.T) {
	br := newTestBridge(t)
	cl, err := br.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	var out bytes.Buffer
	term, err := NewTerminal(cl, &out, 80, 24)
	if err != nil {
		t.Fatalf("NewTerminal: %v", err)
	}
	if err := term.Resize(120, 40); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after Close")
	case err := <-term.Done():
		_ = err // shell exit status or closed-channel error, both fine
	}
}
