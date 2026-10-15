//go:build webssh

package webssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestShellExitClosesWS guards against the dead-session defect: once the
// remote shell exits, the server must close the WebSocket instead of leaving
// the connection (and its session-slot count) alive. A marker file proves the
// exit command really executed; PTY echo alone would fake the marker string
// in the output stream.
func TestShellExitClosesWS(t *testing.T) {
	if strings.EqualFold(runtime.GOOS, "windows") {
		t.Skip("session needs a PTY; run under WSL")
	}
	srv := newTestServer(t, nil)
	c, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	// Sanity: the shell must be alive and echoing before we test exit.
	writeFrame(t, c, msgData, []byte("echo shell-alive\n"))
	ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel1()
	var seen strings.Builder
	for !strings.Contains(seen.String(), "shell-alive") {
		typ, data, err := wsRead(ctx1, t, c)
		if err != nil {
			t.Fatalf("shell not responsive: %v", err)
		}
		if typ == msgData {
			seen.Write(data)
		}
	}

	// Exit the shell.
	marker := filepath.Join(t.TempDir(), "shell-exited")
	writeFrame(t, c, msgData, []byte("touch "+marker+" && exit\n"))
	execDeadline := time.Now().Add(6 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(execDeadline) {
			t.Fatal("shell never executed the exit command")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The shell has exited; the server must close the socket promptly. Any
	// read error other than our own deadline is accepted closure evidence
	// (coder/websocket surfaces the teardown as EOF, not a close frame).
	readCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		_, _, err := wsRead(readCtx, t, c)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("shell exited (marker seen) but WS still open after 8s")
			}
			return
		}
	}
}
