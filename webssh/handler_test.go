//go:build webssh

package webssh

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jpillora/sshd-lite/sshd"
)

func newTestServer(t *testing.T, mutate func(*Options)) *httptest.Server {
	t.Helper()
	opts := Options{Token: "test-token", MaxSessions: 2, IdleTimeout: time.Hour}
	if mutate != nil {
		mutate(&opts)
	}
	h, err := New(opts, sshd.Config{WorkDir: t.TempDir(), LogQuiet: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	srv := httptest.NewServer(h.Routes())
	t.Cleanup(srv.Close)
	return srv
}

func wsDial(t *testing.T, url, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return websocket.Dial(ctx, url+"/webssh?token="+token, nil)
}

func TestTokenRequiredAtConstruction(t *testing.T) {
	if _, err := New(Options{}, sshd.Config{LogQuiet: true}); err == nil {
		t.Fatal("empty token must be rejected")
	}
}

func TestWSRejectsWrongToken(t *testing.T) {
	srv := newTestServer(t, nil)
	_, resp, err := wsDial(t, srv.URL, "wrong")
	if err == nil {
		t.Fatal("dial with wrong token must fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v", resp)
	}
}

func TestWSRejectsWhenFull(t *testing.T) {
	if strings.EqualFold(runtime.GOOS, "windows") {
		t.Skip("session roundtrip needs a PTY; run under WSL")
	}
	srv := newTestServer(t, nil)
	c1, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close(websocket.StatusNormalClosure, "")
	c2, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close(websocket.StatusNormalClosure, "")
	_, resp, err := wsDial(t, srv.URL, "test-token") // MaxSessions=2
	if err == nil {
		t.Fatal("third session must be rejected")
	}
	if resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %v", resp)
	}
}

func TestWSTerminalRoundtrip(t *testing.T) {
	if strings.EqualFold(runtime.GOOS, "windows") {
		t.Skip("PTY roundtrip under WSL; Windows host skips")
	}
	srv := newTestServer(t, nil)
	c, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, encodeFrame(msgData, []byte("echo ws-ok\n"))); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, encodeFrame(msgResize, []byte(`{"cols":100,"rows":30}`))); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageBinary, encodeFrame(msgKeep, nil)); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	deadline := time.After(20 * time.Second)
	for !strings.Contains(got.String(), "ws-ok") {
		typ, data, err := wsRead(ctx, t, c)
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got.String())
		}
		if typ == msgData {
			got.Write(data)
		}
		select {
		case <-deadline:
			t.Fatalf("timeout, output: %q", got.String())
		default:
		}
	}
}

func TestUIServed(t *testing.T) {
	srv := newTestServer(t, nil)
	resp, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "xterm") {
		t.Fatal("index.html must reference xterm")
	}
}

// TestVendorAssets verifies the /assets/ FileServer maps URL paths onto the
// embedded FS (embed root is the package dir, so /assets/... -> assets/...)
// and that the full vendor files made it into the binary.
func TestVendorAssets(t *testing.T) {
	srv := newTestServer(t, nil)
	for _, tc := range []struct{ url, disk, marker string }{
		{"/assets/vendor/xterm.js", "assets/vendor/xterm.js", "xterm"},
		{"/assets/vendor/xterm.css", "assets/vendor/xterm.css", "xterm"},
		{"/assets/vendor/addon-fit.js", "assets/vendor/addon-fit.js", "Fit"},
		{"/assets/app.js", "assets/app.js", "MSG"},
	} {
		t.Run(tc.url, func(t *testing.T) {
			resp, err := srv.Client().Get(srv.URL + tc.url)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d", resp.StatusCode)
			}
			fi, err := os.Stat(tc.disk)
			if err != nil {
				t.Fatalf("stat %s: %v", tc.disk, err)
			}
			if int64(len(body)) != fi.Size() {
				t.Fatalf("%s: served %d bytes, on-disk %d", tc.url, len(body), fi.Size())
			}
			if !strings.Contains(string(body), tc.marker) {
				t.Fatalf("body must contain %q", tc.marker)
			}
		})
	}
}

// wsRead reads one binary frame; t.Fatal on protocol garbage.
func wsRead(ctx context.Context, t *testing.T, c *websocket.Conn) (byte, []byte, error) {
	t.Helper()
	typ, r, err := c.Reader(ctx)
	if err != nil {
		return 0, nil, err
	}
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(r); err != nil {
		return 0, nil, err
	}
	if typ != websocket.MessageBinary {
		return 0, nil, nil // ignore text frames
	}
	return decodeFrame(buf.Bytes())
}
