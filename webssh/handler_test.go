//go:build webssh

package webssh

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// probeUpgrade sends a raw WebSocket upgrade request with the given Origin
// header and returns the HTTP status code (101 on success). Raw TCP mirrors
// what a browser does before any WS framing starts.
func probeUpgrade(t *testing.T, srvURL, token, origin string) int {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET /webssh?token=" + token + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	req += "\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("bad status line: %q", line)
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		t.Fatalf("bad status line: %q", line)
	}
	return code
}

func TestOriginDefaultSameOrigin(t *testing.T) {
	srv := newTestServer(t, nil) // no patterns configured
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Rejection first: successful upgrades below hold session slots for a
	// moment (the cap check runs before Accept's origin check), which would
	// otherwise turn this 403 assertion into a 429.
	if code := probeUpgrade(t, srv.URL, "test-token", "https://evil.example.com"); code != 403 {
		t.Fatalf("cross-origin must be rejected, got %d", code)
	}
	if code := probeUpgrade(t, srv.URL, "test-token", ""); code != 101 {
		t.Fatalf("no-Origin (non-browser client) should upgrade, got %d", code)
	}
	if code := probeUpgrade(t, srv.URL, "test-token", "https://"+u.Host); code != 101 {
		t.Fatalf("same-origin should upgrade, got %d", code)
	}
}

func TestOriginPatternsAllowConfiguredOrigins(t *testing.T) {
	srv := newTestServer(t, func(o *Options) {
		o.OriginPatterns = []string{"*.example.com"}
	})
	if code := probeUpgrade(t, srv.URL, "test-token", "https://evil.other.org"); code != 403 {
		t.Fatalf("origin outside the pattern must stay rejected, got %d", code)
	}
	if code := probeUpgrade(t, srv.URL, "test-token", "https://sub.example.com"); code != 101 {
		t.Fatalf("origin matching the pattern should upgrade, got %d", code)
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

// lockedBuffer is a concurrency-safe log sink: the server logs from its own
// goroutines while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestIdleTimeoutClosesConnection(t *testing.T) {
	if strings.EqualFold(runtime.GOOS, "windows") {
		t.Skip("session needs a PTY; run under WSL")
	}
	var logs lockedBuffer
	srv := newTestServer(t, func(o *Options) {
		o.IdleTimeout = 2 * time.Second
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	c, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	var closeErr error
	for {
		_, _, err := wsRead(ctx, t, c)
		if err != nil {
			closeErr = err
			break
		}
		if time.Since(start) > 15*time.Second {
			t.Fatal("connection not closed by idle watchdog within 15s")
		}
	}
	elapsed := time.Since(start)
	var ce websocket.CloseError
	if !errors.As(closeErr, &ce) || ce.Code != websocket.StatusPolicyViolation {
		t.Fatalf("want policy-violation close, got %v", closeErr)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("closed too late: %v", elapsed)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "session closed") && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	for _, want := range []string{"session open", "idle timeout", "session closed"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("audit log missing %q; logs: %s", want, logs.String())
		}
	}
}
