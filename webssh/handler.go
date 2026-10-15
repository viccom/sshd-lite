//go:build webssh

package webssh

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/jpillora/sshd-lite/sshd"
	"golang.org/x/crypto/ssh"
)

// Options configures the webssh handler. Token is required.
type Options struct {
	Token       string        // required; browsers pass ?token=
	MaxSessions int           // concurrent WS terminal sessions (default 4)
	IdleTimeout time.Duration // drop idle sessions (default 4h)
	Logger      *slog.Logger
}

const (
	wsReadLimit    = 64 << 10 // per WS message, same bound as mole
	writeTimeout   = 10 * time.Second
	keepInterval   = 30 * time.Second
	defaultMaxSess = 4
	defaultIdle    = 4 * time.Hour
)

// Handler serves the browser terminal UI and the /webssh WebSocket.
type Handler struct {
	opts   Options
	bridge *Bridge
	log    *slog.Logger
	active atomic.Int64
}

// New validates options and builds the in-process sshd bridge.
func New(opts Options, base sshd.Config) (*Handler, error) {
	if opts.Token == "" {
		return nil, fmt.Errorf("webssh: token is required (pass --webssh-token or set SSHD_WEBSSH_TOKEN)")
	}
	if opts.MaxSessions <= 0 {
		opts.MaxSessions = defaultMaxSess
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = defaultIdle
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	br, err := NewBridge(base)
	if err != nil {
		return nil, err
	}
	return &Handler{opts: opts, bridge: br, log: opts.Logger}, nil
}

// Close releases the bridge (currently a no-op hook for future resources).
func (h *Handler) Close() {}

// Routes returns the HTTP mux. The "/" UI route is mounted by the embedded
// frontend (assets.go); "/webssh" is the WebSocket endpoint.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /webssh", h.handleWS)
	return mux
}

func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	// Token gate: the loopback keypair is not reachable from outside, so
	// this token is the only door to a shell running as this process.
	got := []byte(r.URL.Query().Get("token"))
	if subtle.ConstantTimeCompare(got, []byte(h.opts.Token)) != 1 {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	if int(h.active.Load()) >= h.opts.MaxSessions {
		http.Error(w, "too many sessions", http.StatusTooManyRequests)
		return
	}
	// websocket.Accept applies same-origin checks by default; non-browser
	// clients without an Origin header are allowed (matches mole).
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	h.active.Add(1)
	defer h.active.Add(-1)
	c.SetReadLimit(wsReadLimit)

	h.serveSession(r, c)
}

// mutexWriter serializes WS writes: terminal pump, keep ticker, and SFTP
// responses all write frames.
type mutexWriter struct {
	c  *websocket.Conn
	mu sync.Mutex
}

func (w *mutexWriter) writeFrame(typ byte, payload []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.Write(ctx, websocket.MessageBinary, encodeFrame(typ, payload))
}

// wsSession couples one browser connection with one loopback SSH client.
type wsSession struct {
	h         *Handler
	c         *websocket.Conn
	w         *mutexWriter
	term      *Terminal
	sshClient *ssh.Client
	lastAct   atomic.Int64 // unix millis
}

func (h *Handler) serveSession(r *http.Request, c *websocket.Conn) {
	sessCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cl, err := h.bridge.Dial()
	if err != nil {
		h.log.Error("webssh: dial bridge", "error", err)
		c.Close(websocket.StatusInternalError, "bridge unavailable")
		return
	}
	defer cl.Close()

	s := &wsSession{
		h: h, c: c, w: &mutexWriter{c: c},
		sshClient: cl,
	}
	s.lastAct.Store(time.Now().UnixMilli())

	// Terminal pump: shell output -> msgData frames.
	termOut := &frameWriter{s: s}
	term, err := NewTerminal(cl, termOut, 80, 24)
	if err != nil {
		h.log.Error("webssh: terminal", "error", err)
		c.Close(websocket.StatusInternalError, "terminal unavailable")
		return
	}
	s.term = term

	h.log.Info("webssh: session open", "remote", r.RemoteAddr)
	defer func() {
		term.Close()
		c.Close(websocket.StatusNormalClosure, "")
		h.log.Info("webssh: session closed", "remote", r.RemoteAddr)
	}()

	// Keep ticker: server-side keepalive every 30s (mole rhythm).
	keepDone := make(chan struct{})
	go func() {
		defer close(keepDone)
		t := time.NewTicker(keepInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.w.writeFrame(msgKeep, nil); err != nil {
					return
				}
			case <-sessCtx.Done():
				return
			}
		}
	}()
	// Idle watchdog.
	go func() {
		for {
			select {
			case <-time.After(keepInterval):
				if time.Since(time.UnixMilli(s.lastAct.Load())) > h.opts.IdleTimeout {
					h.log.Warn("webssh: idle timeout")
					c.Close(websocket.StatusPolicyViolation, "idle timeout")
					return
				}
			case <-sessCtx.Done():
				return
			}
		}
	}()

	// Shell exit -> close WS.
	go func() {
		<-term.Done()
		cancel()
	}()

	// Read loop: browser frames -> terminal / files.
	for {
		rctx, readCancel := context.WithTimeout(context.Background(), time.Hour)
		msgType, reader, err := c.Reader(rctx)
		if err != nil {
			readCancel()
			return // closed, timed out, or protocol error
		}
		buf := make([]byte, 0, 4096)
		b := make([]byte, 4096)
		for {
			n, err := reader.Read(b)
			buf = append(buf, b[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				readCancel()
				return
			}
		}
		readCancel()
		if msgType != websocket.MessageBinary {
			continue
		}
		typ, payload, err := decodeFrame(buf)
		if err != nil {
			h.log.Warn("webssh: bad frame", "error", err)
			return
		}
		s.lastAct.Store(time.Now().UnixMilli())
		switch typ {
		case msgData:
			if _, err := term.Write(payload); err != nil {
				return
			}
		case msgResize:
			var rs struct {
				Cols int `json:"cols"`
				Rows int `json:"rows"`
			}
			if json.Unmarshal(payload, &rs) == nil && rs.Cols > 0 && rs.Rows > 0 {
				_ = term.Resize(rs.Cols, rs.Rows)
			}
		case msgKeep:
			// freshness already recorded
		default:
			s.handleFileMsg(typ, payload) // 0x04-0x0B
		}
	}
}

// frameWriter adapts shell output into msgData frames (io.Writer).
type frameWriter struct{ s *wsSession }

func (f *frameWriter) Write(p []byte) (int, error) {
	if err := f.s.w.writeFrame(msgData, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
