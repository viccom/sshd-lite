//go:build webssh

package webssh

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jpillora/sshd-lite/sshd"
)

type fileTestRig struct {
	srv *httptest.Server
	c   *websocket.Conn
	dir string
}

// newFileRig opens a real WS session against a webssh handler rooted at a
// temp dir. File messages and the terminal stream share the socket; the
// tests only send file frames (the handler's read loop dispatches both).
func newFileRig(t *testing.T) *fileTestRig {
	t.Helper()
	dir := t.TempDir()
	opts := Options{Token: "t", MaxSessions: 2, IdleTimeout: time.Hour}
	h, err := New(opts, sshd.Config{WorkDir: dir, LogQuiet: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, ts.URL+"/webssh?token=t", nil)
	if err != nil {
		t.Fatal(err)
	}
	// 32KiB file chunks + type byte exceed coder/websocket's default
	// client read limit; match the protocol's 64KiB frame bound (§2).
	c.SetReadLimit(wsReadLimit)
	t.Cleanup(func() { c.CloseNow() })
	return &fileTestRig{srv: ts, c: c, dir: dir}
}

// fileAck is the subset of 0x0E payloads the tests assert on.
type fileAck struct {
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
}

// listEntry mirrors one 0x0C files[] item.
type listEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

// listResp mirrors the 0x0C payload.
type listResp struct {
	Path      string      `json:"path"`
	Home      string      `json:"home"`
	Files     []listEntry `json:"files"`
	Truncated bool        `json:"truncated"`
}

// writeFrame sends one binary frame with a 10s deadline.
func writeFrame(t *testing.T, c *websocket.Conn, typ byte, payload []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, encodeFrame(typ, payload)); err != nil {
		t.Fatalf("write frame %#x: %v", typ, err)
	}
}

// waitAck reads frames until the next 0x0E ack, skipping terminal chatter
// (msgData) and keepalives that share the socket.
func waitAck(t *testing.T, ctx context.Context, c *websocket.Conn) fileAck {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		typ, payload, err := wsRead(ctx, t, c)
		if err != nil {
			t.Fatalf("waitAck read: %v", err)
		}
		if typ != msgFileAckResp {
			continue
		}
		var ack fileAck
		if err := json.Unmarshal(payload, &ack); err != nil {
			t.Fatalf("decode ack: %v", err)
		}
		return ack
	}
}

// waitList reads frames until the next 0x0C listing.
func waitList(t *testing.T, ctx context.Context, c *websocket.Conn) listResp {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		typ, payload, err := wsRead(ctx, t, c)
		if err != nil {
			t.Fatalf("waitList read: %v", err)
		}
		if typ != msgFileListResp {
			continue
		}
		var list listResp
		if err := json.Unmarshal(payload, &list); err != nil {
			t.Fatalf("decode list: %v", err)
		}
		return list
	}
}

// waitDownload collects 0x0D data frames until the closing 0x0E ack.
func waitDownload(t *testing.T, ctx context.Context, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var buf bytes.Buffer
	for {
		typ, payload, err := wsRead(ctx, t, c)
		if err != nil {
			t.Fatalf("waitDownload read: %v (got %d bytes)", err, buf.Len())
		}
		switch typ {
		case msgFileDataResp:
			buf.Write(payload)
		case msgFileAckResp:
			var ack fileAck
			if err := json.Unmarshal(payload, &ack); err != nil {
				t.Fatalf("decode ack: %v", err)
			}
			if !ack.OK {
				t.Fatalf("download ack: %v", ack.Msg)
			}
			return buf.Bytes()
		}
	}
}

func listHas(list listResp, name string) bool {
	for _, f := range list.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// chunks splits b into size-byte pieces (last one may be shorter).
func chunks(b []byte, size int) [][]byte {
	var out [][]byte
	for len(b) > size {
		out = append(out, b[:size])
		b = b[size:]
	}
	if len(b) > 0 {
		out = append(out, b)
	}
	return out
}

func TestFileOpsRoundtrip(t *testing.T) {
	rig := newFileRig(t)
	ctx := context.Background()

	// mkdir
	writeFrame(t, rig.c, msgFileMkdirReq, mustJSON(t, map[string]string{"path": filepath.ToSlash(filepath.Join(rig.dir, "sub"))}))
	if ack := waitAck(t, ctx, rig.c); !ack.OK {
		t.Fatalf("mkdir: %v", ack.Msg)
	}

	// upload: req + data + end
	target := filepath.ToSlash(filepath.Join(rig.dir, "sub", "hello.txt"))
	content := bytes.Repeat([]byte("webssh-sftp\n"), 3000) // ~39KB, forces 2 chunks
	writeFrame(t, rig.c, msgFileUploadReq, mustJSON(t, map[string]any{"path": target, "size": len(content)}))
	for _, chunk := range chunks(content, 32768) {
		writeFrame(t, rig.c, msgFileUploadData, chunk)
	}
	writeFrame(t, rig.c, msgFileUploadEnd, nil)
	if ack := waitAck(t, ctx, rig.c); !ack.OK {
		t.Fatalf("upload: %v", ack.Msg)
	}
	// verify on disk directly
	got, err := os.ReadFile(filepath.Join(rig.dir, "sub", "hello.txt"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("uploaded file mismatch: err=%v len=%d", err, len(got))
	}

	// list
	writeFrame(t, rig.c, msgFileListReq, mustJSON(t, map[string]string{"path": filepath.ToSlash(filepath.Join(rig.dir, "sub"))}))
	list := waitList(t, ctx, rig.c)
	if !listHas(list, "hello.txt") {
		t.Fatalf("list missing hello.txt: %+v", list)
	}

	// download
	writeFrame(t, rig.c, msgFileDownloadReq, mustJSON(t, map[string]string{"path": target}))
	data := waitDownload(t, ctx, rig.c)
	if !bytes.Equal(data, content) {
		t.Fatalf("download mismatch: %d bytes", len(data))
	}

	// delete
	writeFrame(t, rig.c, msgFileDeleteReq, mustJSON(t, map[string]string{"path": target}))
	if ack := waitAck(t, ctx, rig.c); !ack.OK {
		t.Fatalf("delete: %v", ack.Msg)
	}
	if _, err := os.Stat(filepath.Join(rig.dir, "sub", "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("file still exists after delete")
	}
}

func TestFileDeleteRejectsRoot(t *testing.T) {
	rig := newFileRig(t)
	writeFrame(t, rig.c, msgFileDeleteReq, mustJSON(t, map[string]string{"path": "/"}))
	ack := waitAck(t, context.Background(), rig.c)
	if ack.OK {
		t.Fatal("delete / must fail")
	}
}
