//go:build webssh

package webssh

import (
	"strings"
	"testing"

	"github.com/jpillora/sshd-lite/sshd"
)

func TestBridgeDialExec(t *testing.T) {
	dir := t.TempDir()
	br, err := NewBridge(sshd.Config{WorkDir: dir, LogQuiet: true})
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	cl, err := br.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cl.Close()

	sess, err := cl.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output("echo webssh-bridge-ok")
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if got := strings.TrimSpace(string(out)); !strings.Contains(got, "webssh-bridge-ok") {
		t.Fatalf("output = %q", got)
	}
}

func TestBridgeMultipleIndependentDials(t *testing.T) {
	br, err := NewBridge(sshd.Config{WorkDir: t.TempDir(), LogQuiet: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		cl, err := br.Dial()
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		if err := cl.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
}
