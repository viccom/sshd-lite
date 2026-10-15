# sshd-lite WebSSH 模块实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在 fork（github.com/viccom/sshd-lite）中新增可开关编译（`-tags webssh`）的 webssh 包，使 sshd-lite 通过浏览器提供 SSH 终端与 SFTP 文件管理，部署于仅有 HTTPS 子域名出口的云端沙箱。

**Architecture:** 浏览器(xterm.js) ⇄ WebSocket(`/webssh?token=`) ⇄ webssh 包（HTTP handler + 会话/文件消息分发）⇄ 进程内桥接（net.Pipe 一端喂 `sshd.Server.HandleConn`，另一端 `ssh.NewClientConn` 用一次性 ed25519 keypair 完成 loopback 握手）⇄ 本进程 sshd 的 PTY shell / SFTP 子系统。WS 线上帧格式与 mole webssh 隧道对齐（`[type 1B][payload]`，消息常量 0x01–0x0F），未来可与 mole 管理面合流。

**Tech Stack:** Go 1.26（本机 go1.26.6 windows/amd64）、`golang.org/x/crypto/ssh`、`github.com/jpillora/sftp`（上游已有依赖）、`github.com/coder/websocket`（新增，唯一新依赖）、`@xterm/xterm@6.0.0` + `@xterm/addon-fit@0.11.0`（vendor 单文件 embed，无 node 构建链）。

---

## 0. 已验证事实（计划编写时全部亲验，执行者无需重查）

| # | 事实 | 证据 |
|---|---|---|
| F1 | fork 已建：`origin=github.com/viccom/sshd-lite`，`upstream=github.com/jpillora/sshd-lite`，本地克隆在 `E:\Go_codes\mole\sshd-lite`，`master` 分支 | 已执行 `gh repo fork` + remote 配置 |
| F2 | 基线 `go build ./...` 通过（go1.26.6，GOPROXY=goproxy.cn） | 已执行，exit=0 |
| F3 | `sshd.Server.HandleConn(net.Conn)` 公开，接受任意 net.Conn（含 net.Pipe），内部含握手并发限制与 10s 握手超时 | `sshd/server_conn.go:14-23` |
| F4 | `sshd.Config` 程序化字段：`KeyBytes []byte`（PEM 私钥）、`AuthKeys []ssh.PublicKey`、`Logger`、`LogQuiet`、`SFTP`、`WorkDir`、`Shell`、`NoClientEnv` 等；`sshd.NewServer(Config) (*Server, error)` | `sshd/config.go:24-77`、`sshd/server.go:48` |
| F5 | 进程内 keypair 先例：`ed25519.GenerateKey` → `ssh.NewSignerFromKey` → `ssh.MarshalPrivateKey` 得 PEM；客户端 `ssh.PublicKeys(userKey)` + `ssh.FixedHostKey(hostKey.PublicKey())` | `example/mosh/main.go:27-67` |
| F6 | `sshd/sshtest` 已有 `sftp.NewClient(*ssh.Client)` 用法（jpillora/sftp fork，API 同 pkg/sftp：ReadDir/Open/Create/RemoveAll/MkdirAll/Stat/Getwd） | `sshd/sshtest/client_sftp.go:170` |
| F7 | main.go 的 flag 解析：`serverCommand` 嵌 `sshd.Config` + `Mosh bool`；`rootBoolFlag`（main.go:196-205）是布尔 flag 白名单，**新布尔 flag 不加进去会被 `normalizeCLIArgs` 吞掉下一个 token** | `main.go:55-59,142-205` |
| F8 | mole 协议常量与 SFTP payload JSON 格式（本计划 §2 全文照录） | `mole/server/internal/api/webssh_handler.go:22-42`、`mole/client/internal/proxy/webssh/handler_sftp.go` |
| F9 | vendor 资源 URL（unpkg，需本机 7897 代理，jsdelivr 不通）全部 206 可用 | 已 curl 亲验 |
| F10 | WSL 测试通道：`wsl.exe -d Ubuntu-24.04 -- bash -ic '...'`，同步用 `cp /mnt/e/... ~/repo/...` 后原生路径跑 | 用户全局 AGENTS.md 沉淀 |

## 1. 设计决策（已定，执行者不再讨论）

- **D1 module 名不改**：fork 保持 `module github.com/jpillora/sshd-lite`，所有 import 用原路径——上游 rebase 零冲突的前提。
- **D2 build tag 隔离**：`webssh/` 包内所有文件带 `//go:build webssh`；main.go 侧用 hook 文件对（`webssh_hook.go` / `webssh_hook_stub.go`）。默认构建零新依赖、行为与上游完全一致。main.go 实际改动仅 3 处（struct 字段、runServer 挂载、rootBoolFlag 一行）。
- **D3 WS 线上格式 = mole 浏览器侧格式**：`[type 1B][payload]`（binary message，无长度前缀——WS 自带帧边界；mole 的 `[len 2B]` 只为 smux 流切帧，不适用）。SFTP 分块沿用 32KB。
- **D4 token 必填**：`--webssh-token`（flag）或 `SSHD_WEBSSH_TOKEN`（env，hook 内自读，不依赖 opts 的 env 机制）。为空时 `webssh.New` 返回错误、进程拒绝启动。校验用 `subtle.ConstantTimeCompare`。
- **D5 认证模型**：loopback keypair 进程内生成、仅存内存；浏览器唯一门禁是 token。Origin 校验用 coder/websocket 默认同源策略（无 Origin 的非浏览器客户端放行）。
- **D6 HTTP 生命周期归 main 侧 hook**：webssh 包只暴露 `New(Options, sshd.Config) (*Handler, error)` 与 `Routes() http.Handler`；ListenAndServe/端口/优雅关闭在 hook 里，包可测试性最好。
- **D7 测试平台矩阵**：协议/桥接/SFTP/认证/超时测试跨平台（Windows 本机直接跑）；PTY 交互测试 `runtime.GOOS=="windows"` 时 skip，WSL（Ubuntu-24.04）跑全量。
- **D8 README 不动**：上游 README 由 `md-tmpl` 生成，改动必冲突。fork 自述文档放 `docs/webssh.md` 新文件。

## 2. 协议规格（对齐 mole webssh，wire-compatible）

```
WS binary message = [type 1B][payload]   payload 为 UTF-8 JSON（文件类）或原始字节（数据类）

0x01 Data           双向   终端字节流（浏览器 stdin / shell stdout）
0x02 Resize         浏览器→服务器  {"cols":int,"rows":int}
0x03 Keep           双向   心跳，接收方忽略即可（服务端每 30s 发；前端每 30s 发）
0x04 FileListReq    浏览器→服务器  {"path":string}   path 空视为 "/"
0x05 FileUploadReq  浏览器→服务器  {"path":string,"size":int}
0x06 FileUploadData 浏览器→服务器  原始分块（≤32768B，多帧）
0x07 FileUploadEnd  浏览器→服务器  无 payload
0x08 FileDownloadReq 浏览器→服务器 {"path":string}
0x09 FileDeleteReq  浏览器→服务器  {"path":string}   拒绝空串与 "/"
0x0A FileMkdirReq   浏览器→服务器  {"path":string}
0x0B FileReadReq    浏览器→服务器  {"path":string}   语义同 download，ack 标 mode:"read"
0x0C FileListResp   服务器→浏览器  {"path":..,"home":..,"files":[{name,size,mode,mod_time,is_dir}..],"truncated"?}
                                    超 65535B 按 3/4 递减截断条目并置 truncated=true
0x0D FileDataResp   服务器→浏览器  原始分块（download/read 的数据流）
0x0E FileAckResp    服务器→浏览器  {"ok":bool,"msg":string,"size"?:int,"name"?:string,"mode"?:string}
0x0F FileReadResp   服务器→浏览器  （保留，本端并入 0x0D/0x0E；常量保留以对齐）
```

会话级参数：服务端 30s 发 Keep；空闲超时默认 4h（收到任何帧刷新，超时断开 WS）；并发会话上限默认 4；WS 读单帧上限 64KB（coder/websocket `ReadLimit`）。

## 3. 目录规划（全部新增，不碰上游既有文件）

```
webssh/
├── protocol.go        消息常量 + encode/decodeFrame
├── bridge.go          进程内 sshd + loopback 拨号
├── terminal.go        PTY shell 会话封装
├── handler.go         Options/Handler/Routes/WS 消息循环/keep/idle/会话计数
├── sftp_ws.go         0x04-0x0B 文件消息处理
├── assets.go          //go:embed assets
├── assets/
│   ├── index.html
│   ├── app.js
│   └── vendor/  xterm.js  xterm.css  addon-fit.js
├── protocol_test.go
├── bridge_test.go
├── terminal_test.go
├── handler_test.go
└── sftp_ws_test.go
（包内全部文件首行 //go:build webssh）
webssh_hook.go         //go:build webssh   startWebSSH 实现
webssh_hook_stub.go    //go:build !webssh  报错 stub
main.go                改 3 处（见 Task 8）
main_test.go           新增（rootBoolFlag/normalizeCLIArgs 单测）
docs/webssh.md         fork 自述文档
docs/plans/2026-10-15-webssh-module.md  本计划
```

---

## Task 0: 特性分支与基线

**Files:** 无新建。

**Step 1: 建分支**

```bash
cd /e/Go_codes/mole/sshd-lite && git checkout -b feature/webssh
```

**Step 2: 基线测试（默认 tag）**

```bash
go test ./... 2>&1 | tail -5; echo "exit=$?"
```

预期：全绿或平台 skip（记录实际输出作为基线；若有 FAIL，停止并上报——不允许在红基线上开工）。

**Step 3: 基线测试（vet）**

```bash
go vet ./... && echo "vet ok"
```

预期：无输出、`vet ok`。

---

## Task 1: build tag 骨架（双 tag 可编译）

**Files:**
- Create: `webssh/webssh.go`
- Create: `webssh_hook.go`
- Create: `webssh_hook_stub.go`

**Step 1: 建 webssh 包占位**（同时引入依赖，验证 tag 隔离下 go.mod 正常）

`webssh/webssh.go`:
```go
//go:build webssh

// Package webssh adds a browser terminal (SSH shell + SFTP) behind a
// WebSocket endpoint. It is only compiled with the "webssh" build tag.
package webssh
```

`webssh_hook.go`:
```go
//go:build webssh

package main

import (
	"context"
	"errors"
	"os"

	"github.com/jpillora/sshd-lite/sshd"
)

// startWebSSH starts the browser terminal HTTP server when built with
// -tags webssh. It runs until ctx is cancelled.
func startWebSSH(ctx context.Context, token, port string, base sshd.Config) error {
	if token == "" {
		token = os.Getenv("SSHD_WEBSSH_TOKEN")
	}
	_ = port
	return errors.New("webssh: not implemented yet: " + token)
}
```

`webssh_hook_stub.go`:
```go
//go:build !webssh

package main

import (
	"context"
	"errors"

	"github.com/jpillora/sshd-lite/sshd"
)

// startWebSSH errors out: the binary was built without the webssh tag.
func startWebSSH(ctx context.Context, token, port string, base sshd.Config) error {
	return errors.New("--webssh requires a build with -tags webssh")
}
```

**Step 2: 引入 WebSocket 依赖**

```bash
cd /e/Go_codes/mole/sshd-lite && go get github.com/coder/websocket@latest 2>&1 | tail -3
```

预期：`go: added github.com/coder/websocket vX.Y.Z`（记录版本号）。

**Step 3: 验证双 tag 编译**

```bash
go build ./... && go build -tags webssh . && go vet ./... && go vet -tags webssh ./webssh/ && echo ALL-OK
```

预期：`ALL-OK`。若默认构建因 coder/websocket 报错，说明 tag 隔离写错（检查每个 webssh 文件首行）。

**Step 4: Commit**

```bash
git add webssh/ webssh_hook.go webssh_hook_stub.go go.mod go.sum
git commit -m "feat(webssh): add build-tag skeleton for browser terminal module"
```

---

## Task 2: 帧协议层（TDD）

**Files:**
- Create: `webssh/protocol.go`
- Test: `webssh/protocol_test.go`

**Step 1: 写失败测试**

`webssh/protocol_test.go`:
```go
//go:build webssh

package webssh

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeFrame(t *testing.T) {
	for _, tc := range []struct {
		name    string
		typ     byte
		payload []byte
	}{
		{"data empty", msgData, nil},
		{"data bytes", msgData, []byte("hello")},
		{"resize json", msgResize, []byte(`{"cols":80,"rows":24}`)},
		{"file chunk", msgFileUploadData, bytes.Repeat([]byte{0xAB}, 32768)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := encodeFrame(tc.typ, tc.payload)
			typ, payload, err := decodeFrame(frame)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if typ != tc.typ {
				t.Fatalf("type = %#x, want %#x", typ, tc.typ)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Fatalf("payload mismatch: %d bytes, want %d", len(payload), len(tc.payload))
			}
		})
	}
}

func TestDecodeFrameInvalid(t *testing.T) {
	if _, _, err := decodeFrame(nil); err == nil {
		t.Fatal("empty frame must error")
	}
	if _, _, err := decodeFrame([]byte{0x00}); err == nil {
		t.Fatal("unknown type 0x00 must error")
	}
	if _, _, err := decodeFrame([]byte{0x42, 'x'}); err == nil {
		t.Fatal("unknown type 0x42 must error")
	}
	if _, _, err := decodeFrame([]byte{msgData}); err != nil {
		t.Fatalf("type-only frame should be valid: %v", err)
	}
}
```

**Step 2: 跑红**

```bash
go test -tags webssh ./webssh/ -run TestEncodeDecodeFrame -v 2>&1 | tail -5
```

预期：编译失败 `undefined: encodeFrame`（及相关常量）。

**Step 3: 最小实现**

`webssh/protocol.go`:
```go
//go:build webssh

package webssh

import "fmt"

// Message types. Wire-compatible with mole's webssh tunnel protocol
// (mole server/internal/api/webssh_handler.go) so the mole admin UI can
// connect to this endpoint unchanged in the future.
const (
	msgData   byte = 0x01 // terminal payload
	msgResize byte = 0x02 // {"cols":..,"rows":..}
	msgKeep   byte = 0x03 // keepalive; receiver ignores

	msgFileListReq     byte = 0x04 // {"path"}
	msgFileUploadReq   byte = 0x05 // {"path","size"}
	msgFileUploadData  byte = 0x06 // raw chunk (<=32KiB)
	msgFileUploadEnd   byte = 0x07 // no payload
	msgFileDownloadReq byte = 0x08 // {"path"}
	msgFileDeleteReq   byte = 0x09 // {"path"}
	msgFileMkdirReq    byte = 0x0A // {"path"}
	msgFileReadReq     byte = 0x0B // {"path"} — like download, ack has mode:"read"

	msgFileListResp byte = 0x0C // {"path","home","files":[..],"truncated"?}
	msgFileDataResp byte = 0x0D // raw chunk (download/read stream)
	msgFileAckResp  byte = 0x0E // {"ok","msg","size"?,"name"?,"mode"?}
	msgFileReadResp byte = 0x0F // reserved; constant kept for mole parity
)

// validTypes lists every accepted message type byte.
var validTypes = map[byte]bool{
	msgData: true, msgResize: true, msgKeep: true,
	msgFileListReq: true, msgFileUploadReq: true, msgFileUploadData: true,
	msgFileUploadEnd: true, msgFileDownloadReq: true, msgFileDeleteReq: true,
	msgFileMkdirReq: true, msgFileReadReq: true,
	msgFileListResp: true, msgFileDataResp: true, msgFileAckResp: true, msgFileReadResp: true,
}

// encodeFrame packs one WS binary message: [type 1B][payload].
func encodeFrame(typ byte, payload []byte) []byte {
	frame := make([]byte, 1+len(payload))
	frame[0] = typ
	copy(frame[1:], payload)
	return frame
}

// decodeFrame splits one WS binary message. Empty frames and unknown
// message types are rejected.
func decodeFrame(frame []byte) (byte, []byte, error) {
	if len(frame) == 0 {
		return 0, nil, fmt.Errorf("webssh: empty frame")
	}
	typ := frame[0]
	if !validTypes[typ] {
		return 0, nil, fmt.Errorf("webssh: unknown frame type %#x", typ)
	}
	return typ, frame[1:], nil
}
```

**Step 4: 跑绿**

```bash
go test -tags webssh ./webssh/ -v 2>&1 | tail -5
```

预期：PASS（2 个测试）。

**Step 5: Commit**

```bash
git add webssh/protocol.go webssh/protocol_test.go
git commit -m "feat(webssh): frame codec aligned with mole webssh wire format"
```

---

## Task 3: 进程内桥接（TDD，跨平台）

**Files:**
- Create: `webssh/bridge.go`
- Test: `webssh/bridge_test.go`

**Step 1: 写失败测试**

`webssh/bridge_test.go`:
```go
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
```

**Step 2: 跑红**

```bash
go test -tags webssh ./webssh/ -run TestBridge -v 2>&1 | tail -5
```

预期：编译失败 `undefined: NewBridge`。

**Step 3: 实现**

`webssh/bridge.go`:
```go
//go:build webssh

package webssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net"
	"time"

	"github.com/jpillora/sshd-lite/sshd"
	"golang.org/x/crypto/ssh"
)

// Bridge owns an in-process sshd and dials it over net.Pipe using a
// one-time ed25519 keypair generated at startup. The keys exist only in
// process memory; the browser's sole gate is the webssh token.
type Bridge struct {
	server  *sshd.Server
	userKey ssh.Signer
	hostKey ssh.PublicKey
}

// NewBridge builds the hidden sshd from base, forcing the SFTP subsystem
// on (the file panel needs it) and replacing auth with a programmatic key.
func NewBridge(base sshd.Config) (*Bridge, error) {
	hostPEM, hostSigner, err := genKey()
	if err != nil {
		return nil, fmt.Errorf("webssh: host key: %w", err)
	}
	_, userSigner, err := genKey()
	if err != nil {
		return nil, fmt.Errorf("webssh: user key: %w", err)
	}
	base.KeyBytes = hostPEM
	base.AuthKeys = []ssh.PublicKey{userSigner.PublicKey()}
	base.SFTP = true
	base.TCPForwarding = false
	base.AuthType = "" // AuthKeys and AuthType are mutually exclusive
	s, err := sshd.NewServer(base)
	if err != nil {
		return nil, fmt.Errorf("webssh: new server: %w", err)
	}
	return &Bridge{server: s, userKey: userSigner, hostKey: hostSigner.PublicKey()}, nil
}

func genKey() ([]byte, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, nil, err
	}
	return pemEncode(block), signer, nil
}

// Dial opens one loopback SSH connection: the server half of a net.Pipe
// feeds Server.HandleConn in a goroutine (it serves until the connection
// closes); the client half completes a pinned-hostkey handshake.
func (b *Bridge) Dial() (*ssh.Client, error) {
	clientConn, serverConn := net.Pipe()
	go b.server.HandleConn(serverConn)
	cfg := &ssh.ClientConfig{
		User:            "webssh",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(b.userKey)},
		HostKeyCallback: ssh.FixedHostKey(b.hostKey),
		Timeout:         10 * time.Second,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(clientConn, "loopback", cfg)
	if err != nil {
		clientConn.Close()
		return nil, fmt.Errorf("webssh: loopback handshake: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}
```

注意：`pemEncode(block)` 需要落为 `pem.EncodeToMemory(block)`（直接内联，不留辅助函数）。若 `ssh.MarshalPrivateKey` 不存在于所用 x/crypto 版本，回退用 `example/mosh/main.go:27-41` 同款实现（该文件已在仓库中验证可编译——直接照抄该函数即可）。

**Step 4: 跑绿**

```bash
go test -tags webssh ./webssh/ -run TestBridge -v 2>&1 | tail -8
```

预期：2 PASS。exec 走 sshd-lite 的 executeCommand，Windows（默认 shell）与 Linux 输出都含 `webssh-bridge-ok` 字面量。

**Step 5: Commit**

```bash
git add webssh/bridge.go webssh/bridge_test.go
git commit -m "feat(webssh): in-process sshd bridge over net.Pipe with one-time keypair"
```

---

## Task 4: PTY 终端会话封装（TDD，PTY 测试 WSL 全跑/Windows skip）

**Files:**
- Create: `webssh/terminal.go`
- Test: `webssh/terminal_test.go`

**Step 1: 写失败测试**

`webssh/terminal_test.go`:
```go
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
		t.Fatal(err)
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
	case err := <-termDone(term):
		_ = err // shell exit status or closed-channel error, both fine
	}
}
```

`termDone` 是测试辅助：让 Terminal 暴露 `Done() <-chan error`（见实现），测试里直接 `term.Done()`，删掉 `termDone` 包装。

**Step 2: 跑红**

```bash
go test -tags webssh ./webssh/ -run TestTerminal -v 2>&1 | tail -5
```

预期：编译失败 `undefined: NewTerminal`。

**Step 3: 实现**

`webssh/terminal.go`:
```go
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
	sess *ssh.Session
	done chan error
	once sync.Once
}

// NewTerminal starts a PTY shell on client, mirroring both stdout and
// stderr into out. cols/rows fall back to 80x24 when non-positive.
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
	sess.Stdout = out
	sess.Stderr = out
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
	t := &Terminal{sess: sess, done: make(chan error, 1)}
	go func() { t.done <- sess.Wait() }()
	return t, nil
}

// Write feeds terminal stdin (browser keystrokes).
func (t *Terminal) Write(p []byte) (int, error) { return t.sess.Stdin.Write(p) }

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
```

**Step 4: 跑绿（Windows 本机将 skip，Step 5 WSL 全跑）**

```bash
go test -tags webssh ./webssh/ -run TestTerminal -v 2>&1 | tail -6
```

预期：Windows 上 SKIP×2；WSL 上 PASS×2（Step 5）。

**Step 5: WSL 全量验证**

```bash
wsl.exe -d Ubuntu-24.04 -- bash -ic 'mkdir -p ~/sshd-lite && cp -r /mnt/e/Go_codes/mole/sshd-lite/webssh ~/sshd-lite/ 2>/dev/null; cp /mnt/e/Go_codes/mole/sshd-lite/{go.mod,go.sum} ~/sshd-lite/ 2>/dev/null; cd ~/sshd-lite && sed -i "s/\r$//" webssh/*.go && go mod tidy && go test -tags webssh ./webssh/ -v' 2>&1 | grep -vE 'job control|nvm|rustup' | tail -12
```

预期：`TestTerminalEcho PASS`、`TestTerminalResizeAndClose PASS`、`TestBridge* PASS`。注意：首次 WSL 跑需完整模块上下文；若 `go mod tidy` 因缺上游包报错，改为整仓同步 `cp -r /mnt/e/Go_codes/mole/sshd-lite/{*.go,sshd,xssh,client,mosh,internal,docs,go.mod,go.sum} ~/sshd-lite/` 后再跑（在 `~/sshd-lite` 原生路径执行）。

**Step 6: Commit**

```bash
git add webssh/terminal.go webssh/terminal_test.go
git commit -m "feat(webssh): PTY terminal session wrapper with resize and bounded teardown"
```

---

## Task 5: WS handler —— token/上限/双向泵/keep（TDD）

**Files:**
- Create: `webssh/handler.go`
- Test: `webssh/handler_test.go`

**Step 1: 写失败测试**（认证与上限跨平台；终端往返 WSL）

`webssh/handler_test.go`:
```go
//go:build webssh

package webssh

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	if strings.EqualFold(runtimeGOOS(), "windows") {
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
	if strings.EqualFold(runtimeGOOS(), "windows") {
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
```

`runtimeGOOS()` 直接用 `runtime.GOOS`（import runtime），不要包装函数。

**Step 2: 跑红**

```bash
go test -tags webssh ./webssh/ -run 'TestToken|TestWS' -v 2>&1 | tail -5
```

预期：编译失败 `undefined: New` / `undefined: Options`。

**Step 3: 实现**

`webssh/handler.go`:
```go
//go:build webssh

package webssh

import (
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
)

// Options configures the webssh handler. Token is required.
type Options struct {
	Token       string        // required; browsers pass ?token=
	MaxSessions int           // concurrent WS terminal sessions (default 4)
	IdleTimeout time.Duration // drop idle sessions (default 4h)
	Logger      *slog.Logger
}

const (
	wsReadLimit     = 64 << 10 // per WS message, same bound as mole
	writeTimeout    = 10 * time.Second
	keepInterval    = 30 * time.Second
	defaultMaxSess  = 4
	defaultIdle     = 4 * time.Hour
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

// Routes returns the HTTP mux: "/" serves the embedded UI, "/webssh" the WS.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.serveUI)
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
	h        *Handler
	c        *websocket.Conn
	w        *mutexWriter
	term     *Terminal
	client   *sshClientWrapper // *ssh.Client plus lazy SFTP
	lastAct  atomic.Int64      // unix millis
}
```

（上面的 `sshClientWrapper` 不需要——直接字段 `sshClient *ssh.Client` + sftp_ws.go 里的懒建逻辑挂 session 上。写实现时落到 `serveSession`：）

```go
func (h *Handler) serveSession(r *http.Request, c *websocket.Conn) {
	ctx := c.CloseRead(context.Background()) // real ctx comes from Reader below
	_ = ctx
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
```

实现注意（执行者落实时清理）：
- `serveSession` 开头两行 `c.CloseRead` 的占位删除——读循环就在本函数，别用 CloseRead。
- `wsSession` 字段定型为：`h *Handler`、`c *websocket.Conn`、`w *mutexWriter`、`term *Terminal`、`sshClient *ssh.Client`、`lastAct atomic.Int64`（import `golang.org/x/crypto/ssh`）。
- 读循环每帧 1 小时 ctx 只是保险；真正会话时长由 idle watchdog 与 shell 退出控制。
- `frameWriter` 必须在 `NewTerminal` 之前构造并传给 `termOut` 变量（上面代码已如此）。

**Step 4: 跑绿（Windows 跑认证/构造；终端用例 WSL）**

```bash
go test -tags webssh ./webssh/ -v 2>&1 | tail -10
```

预期（Windows）：`TestTokenRequiredAtConstruction`、`TestWSRejectsWrongToken` PASS；`TestWSRejectsWhenFull`、`TestWSTerminalRoundtrip` SKIP（此时尚无 sftp 处理，文件消息走 default 分支调 `s.handleFileMsg` 会编译失败——本 task 先建 `webssh/sftp_ws.go` 的空骨架 `func (s *wsSession) handleFileMsg(typ byte, payload []byte) {}`，Task 6 再填充）。

**Step 5: WSL 跑全量**

同 Task 4 Step 5 命令。预期 4 个用例全 PASS。

**Step 6: Commit**

```bash
git add webssh/handler.go webssh/handler_test.go webssh/sftp_ws.go
git commit -m "feat(webssh): websocket handler with token gate, session cap, keep and idle watchdog"
```

---

## Task 6: SFTP 文件操作（TDD，跨平台）

**Files:**
- Modify: `webssh/sftp_ws.go`（替换空骨架）
- Test: `webssh/sftp_ws_test.go`

**Step 1: 写失败测试**

`webssh/sftp_ws_test.go`:
```go
//go:build webssh

package webssh

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fileTestRig struct {
	srv *httptestServerAlias
	c   *websocket.Conn
	dir string
}

func newFileRig(t *testing.T) *fileTestRig {
	t.Helper()
	dir := t.TempDir()
	opts := Options{Token: "t", MaxSessions: 2, IdleTimeout: time.Hour}
	h, err := New(opts, testBaseConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	ts := httptestNewServer(h.Routes())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, ts.URL+"/webssh?token=t", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &fileRigAlias{srv: ts, c: c, dir: dir}
}

// send/write/read helpers omitted here for brevity: reuse wsRead from
// handler_test.go, add writeFrame helper that wraps c.Write with a 10s ctx.
```

（执行者注意：上面的 alias 类型名不存在——直接写真实类型 `*httptest.Server` 与 `*fileTestRig`，helper 从 `handler_test.go` 复用 `wsRead`，另加 `writeFrame(t, c, typ, payload)`。测试主体：）

```go
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
	for chunk := range chunks(content, 32768) {
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
	ack := waitAck(context.Background(), t, rig.c)
	if ack.OK {
		t.Fatal("delete / must fail")
	}
}
```

（helper 实现：`writeFrame` 带 10s ctx 的 `c.Write(binary, encodeFrame(...))`；`waitAck` 循环 `wsRead` 直到 `msgFileAckResp` 后 JSON 解码 `{ok,msg}`；`waitList` 等 `msgFileListResp`；`waitDownload` 收集 `msgFileDataResp` 直到 `msgFileAckResp` 且 ok；`chunks` 是 `func chunks(b []byte, size int) iter.Seq[[]byte]`；`mustJSON` 包装 `json.Marshal`。）

**Step 2: 跑红**

```bash
go test -tags webssh ./webssh/ -run TestFile -v 2>&1 | tail -5
```

预期：FAIL（mkdir ack 不来，超时）——骨架 `handleFileMsg` 为空。

**Step 3: 实现**（移植 mole `handler_sftp.go` 语义，见 §2 规格）

`webssh/sftp_ws.go`:
```go
//go:build webssh

package webssh

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"

	"github.com/jpillora/sftp"
)

// sftp lazy-builds the SFTP client on the session's loopback SSH client.
// It is single-session: the mutex guards concurrent file requests.
func (s *wsSession) sftpClient() (*sftp.Client, error) {
	s.sftpMu.Lock()
	defer s.sftpMu.Unlock()
	if s.sftp != nil {
		if _, err := s.sftp.Getwd(); err == nil {
			return s.sftp, nil
		}
		s.sftp.Close()
		s.sftp = nil
	}
	sc, err := sftp.NewClient(s.sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp new client: %w", err)
	}
	s.sftp = sc
	return sc, nil
}

// wsSession gains fields: sftp *sftp.Client; sftpMu sync.Mutex

func (s *wsSession) handleFileMsg(typ byte, payload []byte) {
	switch typ {
	case msgFileListReq:
		s.fileList(payload)
	case msgFileUploadReq, msgFileUploadData, msgFileUploadEnd:
		s.fileUpload(typ, payload)
	case msgFileDownloadReq:
		s.fileDownload(payload, "download")
	case msgFileReadReq:
		s.fileDownload(payload, "read")
	case msgFileDeleteReq:
		s.fileDelete(payload)
	case msgFileMkdirReq:
		s.fileMkdir(payload)
	}
}

func (s *wsSession) ack(ok bool, msg string, extra map[string]any) {
	m := map[string]any{"ok": ok, "msg": msg}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	_ = s.w.writeFrame(msgFileAckResp, b)
}

func (s *wsSession) fileList(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil {
		s.ack(false, "invalid request", nil)
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	files, err := sc.ReadDir(req.Path)
	if err != nil {
		s.ack(false, fmt.Sprintf("read dir: %v", err), nil)
		return
	}
	home, _ := sc.Getwd()
	if home == "" {
		home = "/"
	}
	type entry struct {
		Name    string `json:"name"`
		Size    int64  `json:"size"`
		Mode    string `json:"mode"`
		ModTime string `json:"mod_time"`
		IsDir   bool   `json:"is_dir"`
	}
	entries := make([]entry, 0, len(files))
	for _, f := range files {
		entries = append(entries, entry{
			Name: f.Name(), Size: f.Size(), Mode: f.Mode().String(),
			ModTime: f.ModTime().Format("2006-01-02 15:04:05"), IsDir: f.IsDir(),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	build := func(es []entry, truncated bool) ([]byte, error) {
		m := map[string]any{"path": req.Path, "home": home, "files": es}
		if truncated {
			m["truncated"] = true
		}
		return json.Marshal(m)
	}
	resp, err := build(entries, false)
	const maxPayload = 65535
	for err == nil && len(resp) > maxPayload && len(entries) > 1 {
		entries = entries[:len(entries)*3/4]
		resp, err = build(entries, true)
	}
	if err != nil {
		s.ack(false, "marshal list", nil)
		return
	}
	_ = s.w.writeFrame(msgFileListResp, resp)
}

func (s *wsSession) fileUpload(typ byte, payload []byte) {
	switch typ {
	case msgFileUploadReq:
		var req struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		}
		if json.Unmarshal(payload, &req) != nil || req.Path == "" {
			s.ack(false, "invalid upload request", nil)
			return
		}
		sc, err := s.sftpClient()
		if err != nil {
			s.ack(false, err.Error(), nil)
			return
		}
		f, err := sc.Create(req.Path)
		if err != nil {
			s.ack(false, fmt.Sprintf("create file: %v", err), nil)
			return
		}
		s.uploadMu.Lock()
		s.uploadFile = f
		s.uploadErr = nil
		s.uploadMu.Unlock()
	case msgFileUploadData:
		s.uploadMu.Lock()
		if s.uploadFile != nil && len(payload) > 0 {
			_, s.uploadErr = s.uploadFile.Write(payload)
		}
		s.uploadMu.Unlock()
	case msgFileUploadEnd:
		s.uploadMu.Lock()
		f := s.uploadFile
		werr := s.uploadErr
		s.uploadFile = nil
		s.uploadMu.Unlock()
		if f == nil {
			s.ack(false, "no upload in progress", nil)
			return
		}
		var size int64
		if st, err := f.Stat(); err == nil {
			size = st.Size()
		}
		f.Close()
		if werr != nil {
			s.ack(false, fmt.Sprintf("write error: %v", werr), nil)
			return
		}
		s.ack(true, "upload completed", map[string]any{"size": size})
	}
}

func (s *wsSession) fileDownload(payload []byte, mode string) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" {
		s.ack(false, "invalid request", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	f, err := sc.Open(req.Path)
	if err != nil {
		s.ack(false, fmt.Sprintf("open file: %v", err), nil)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.ack(false, fmt.Sprintf("stat file: %v", err), nil)
		return
	}
	buf := make([]byte, 32768)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if werr := s.w.writeFrame(msgFileDataResp, buf[:n]); werr != nil {
				return
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			s.ack(false, fmt.Sprintf("read file: %v", err), nil)
			return
		}
	}
	extra := map[string]any{"size": st.Size(), "name": filepath.Base(req.Path)}
	if mode == "read" {
		extra["mode"] = "read"
	}
	s.ack(true, mode+" completed", extra)
}

func (s *wsSession) fileDelete(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" || req.Path == "/" {
		s.ack(false, "invalid path", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	if err := sc.RemoveAll(req.Path); err != nil {
		s.ack(false, fmt.Sprintf("delete: %v", err), nil)
		return
	}
	s.ack(true, "deleted", nil)
}

func (s *wsSession) fileMkdir(payload []byte) {
	var req struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(payload, &req) != nil || req.Path == "" {
		s.ack(false, "invalid request", nil)
		return
	}
	sc, err := s.sftpClient()
	if err != nil {
		s.ack(false, err.Error(), nil)
		return
	}
	if err := sc.MkdirAll(req.Path); err != nil {
		s.ack(false, fmt.Sprintf("mkdir: %v", err), nil)
		return
	}
	s.ack(true, "created", nil)
}
```

（`wsSession` 补字段：`sftp *sftp.Client`、`sftpMu sync.Mutex`、`uploadFile *sftp.File`、`uploadErr error`、`uploadMu sync.Mutex`。）

**Step 4: 跑绿（跨平台，Windows 直接全跑）**

```bash
go test -tags webssh ./webssh/ -run TestFile -v 2>&1 | tail -6
```

预期：2 PASS。SFTP 不依赖 PTY，Windows（内置 sftp 子系统）可用；若 Windows 下 sftp 子系统行为异常，WSL 验证并在提交信息注明平台矩阵。

**Step 5: Commit**

```bash
git add webssh/sftp_ws.go webssh/sftp_ws_test.go webssh/handler.go
git commit -m "feat(webssh): SFTP file ops over WS (list/upload/download/read/delete/mkdir), mole-compatible payloads"
```

---

## Task 7: 内嵌前端（页面 + vendor 资源）

**Files:**
- Create: `webssh/assets.go`
- Create: `webssh/assets/index.html`
- Create: `webssh/assets/app.js`
- Create: `webssh/assets/vendor/{xterm.js,xterm.css,addon-fit.js}`（curl 下载）
- Test: `webssh/handler_test.go` 追加 UI 冒烟

**Step 1: 下载 vendor 资源**（URL 已亲验 206，需代理）

```bash
cd /e/Go_codes/mole/sshd-lite && mkdir -p webssh/assets/vendor && export https_proxy=http://127.0.0.1:7897 && \
curl -fsSL -o webssh/assets/vendor/xterm.js https://unpkg.com/@xterm/xterm@6.0.0/lib/xterm.js && \
curl -fsSL -o webssh/assets/vendor/xterm.css https://unpkg.com/@xterm/xterm@6.0.0/css/xterm.css && \
curl -fsSL -o webssh/assets/vendor/addon-fit.js https://unpkg.com/@xterm/addon-fit@0.11.0/lib/addon-fit.js && \
head -c 120 webssh/assets/vendor/xterm.js && echo && wc -c webssh/assets/vendor/*
```

预期：三个文件均下载成功且非空（xterm.js 约 280KB）。

**Step 2: 写失败测试**（追加到 `webssh/handler_test.go`）

```go
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
```

**Step 3: 跑红**

```bash
go test -tags webssh ./webssh/ -run TestUIServed -v 2>&1 | tail -3
```

预期：FAIL（404，serveUI 未实现）。

**Step 4: 实现 embed 与页面**

`webssh/assets.go`:
```go
//go:build webssh

package webssh

import (
	"embed"
	"net/http"
)

//go:embed assets
var assetsFS embed.FS

func (h *Handler) serveUI(w http.ResponseWriter, r *http.Request) {
	fs := http.FS(assetsFS)
	f, err := fs.Open("assets/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	stat, _ := f.Stat()
	http.ServeContent(w, r, "index.html", stat.ModTime(), f.(readSeeker))
}
```

（`readSeeker` 不存在——`http.FS` 返回 `http.File` 已实现 `io.ReadSeeker`，直接传 `f` 即可；`serveUI` 里多余类型断言删除。）

`webssh/assets/index.html`:
```html
<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sshd-lite webssh</title>
<link rel="stylesheet" href="assets/vendor/xterm.css">
<style>
  html, body { margin: 0; height: 100%; background: #1e1e1e; color: #ccc;
    font-family: system-ui, sans-serif; display: flex; flex-direction: column; }
  #bar { display: flex; gap: 8px; padding: 6px 10px; background: #2d2d2d;
    align-items: center; font-size: 13px; }
  #bar button { background: #3c3c3c; color: #ccc; border: 1px solid #555;
    border-radius: 4px; padding: 3px 10px; cursor: pointer; }
  #bar button:hover { background: #505050; }
  #term { flex: 1; min-height: 0; padding: 4px; }
  #files { display: none; height: 40%; border-top: 2px solid #444;
    overflow: auto; background: #252526; padding: 8px; font-size: 13px; }
  #files table { border-collapse: collapse; width: 100%; }
  #files td, #files th { text-align: left; padding: 2px 8px; }
  #files tr:hover { background: #2a2d2e; }
  #status { margin-left: auto; color: #8f8; }
  #status.err { color: #f66; }
  #drop { position: fixed; inset: 0; display: none; align-items: center;
    justify-content: center; background: rgba(0,0,0,.6); font-size: 24px; }
  #drop.on { display: flex; }
</style>
</head>
<body>
<div id="bar">
  <button id="toggle-files">文件</button>
  <button id="up">上传</button>
  <button id="mkdir">新建目录</button>
  <span id="cwd"></span>
  <span id="status">connecting…</span>
</div>
<div id="term"></div>
<div id="files"></div>
<div id="drop">松开以上传到当前目录</div>
<script src="assets/vendor/xterm.js"></script>
<script src="assets/vendor/addon-fit.js"></script>
<script src="app.js"></script>
</body>
</html>
```

`webssh/assets/app.js`（协议常量与 §2 一致）:
```js
"use strict";
const MSG = { DATA: 0x01, RESIZE: 0x02, KEEP: 0x03,
  LIST_REQ: 0x04, UPLOAD_REQ: 0x05, UPLOAD_DATA: 0x06, UPLOAD_END: 0x07,
  DOWNLOAD_REQ: 0x08, DELETE_REQ: 0x09, MKDIR_REQ: 0x0A, READ_REQ: 0x0B,
  LIST_RESP: 0x0C, DATA_RESP: 0x0D, ACK_RESP: 0x0E, READ_RESP: 0x0F };

const qs = new URLSearchParams(location.search);
let token = qs.get("token") || localStorage.getItem("webssh_token") || "";
while (!token) {
  token = prompt("webssh token:") || "";
  if (token) localStorage.setItem("webssh_token", token);
}
let cwd = ".";
const statusEl = document.getElementById("status");
const filesEl = document.getElementById("files");
const cwdEl = document.getElementById("cwd");

const term = new Terminal({ cursorBlink: true, theme: { background: "#1e1e1e" } });
const fit = new FitAddon.FitAddon();
term.loadAddon(fit);
term.open(document.getElementById("term"));
fit.fit();

let dl = null; // active download accumulator {name, chunks}

const ws = new WebSocket(
  (location.protocol === "https:" ? "wss://" : "ws://") + location.host +
  "/webssh?token=" + encodeURIComponent(token));
ws.binaryType = "arraybuffer";

function frame(t, payload) {
  const body = payload ? new TextEncoder().encode(payload) : new Uint8Array(0);
  const buf = new Uint8Array(1 + body.length);
  buf[0] = t; buf.set(body, 1);
  return buf;
}
function frameRaw(t, bytes) {
  const buf = new Uint8Array(1 + bytes.length);
  buf[0] = t; buf.set(bytes, 1);
  return buf;
}
function send(t, payload) { if (ws.readyState === 1) ws.send(frame(t, payload)); }

ws.onopen = () => { statusEl.textContent = "online"; sendResize(); setInterval(() => send(MSG.KEEP), 30000); };
ws.onclose = (e) => { statusEl.textContent = "disconnected"; statusEl.className = "err"; };
ws.onerror = () => { statusEl.textContent = "error"; statusEl.className = "err"; };

ws.onmessage = (ev) => {
  const b = new Uint8Array(ev.data);
  const t = b[0], payload = b.subarray(1);
  switch (t) {
    case MSG.DATA: term.write(new Text8Decoder().decode(payload)); break;
    case MSG.KEEP: break;
    case MSG.LIST_RESP: renderList(JSON.parse(new Text8Decoder().decode(payload))); break;
    case MSG.DATA_RESP:
      if (dl) dl.chunks.push(payload); break;
    case MSG.ACK_RESP: handleAck(JSON.parse(new Text8Decoder().decode(payload))); break;
  }
};
const Text8Decoder = TextDecoder; // alias for readability
```

（执行者注意：`new Text8Decoder()` 写法错误——先 `const dec = new TextDecoder()` 一次复用。）

```js
function sendResize() { send(MSG.RESIZE, JSON.stringify({ cols: term.cols, rows: term.rows })); }
window.addEventListener("resize", () => { fit.fit(); sendResize(); });

term.onData((d) => send(MSG.DATA, d));
// xtermjs strings are UTF-16; frame() encodes them — send raw instead:
// replace the DATA path with ws.send(frameRaw(MSG.DATA, new TextEncoder().encode(d)))

function handleAck(ack) {
  if (!ack.ok) { statusEl.textContent = "⚠ " + ack.msg; statusEl.className = "err"; return; }
  statusEl.className = "";
  if (ack.mode === "download" || ack.mode === "read") {
    if (dl) {
      const blob = new Blob(dl.chunks);
      const a = document.createElement("a");
      a.href = URL.createObjectURL(blob);
      a.download = dl.name; a.click();
      URL.revokeObjectURL(a.href);
      dl = null;
    }
  }
  if (filesEl.style.display !== "none") listDir(cwd);
}

function listDir(path) { send(MSG.LIST_REQ, JSON.stringify({ path })); }

function renderList(resp) {
  cwd = resp.path; cwdEl.textContent = cwd;
  let rows = resp.files.map((f) =>
    `<tr data-name="${esc(f.name)}" data-dir="${f.is_dir}">
       <td>${f.is_dir ? "📁" : "📄"} ${esc(f.name)}</td>
       <td>${f.is_dir ? "-" : f.size}</td><td>${f.mode}</td><td>${f.mod_time}</td>
       <td><button class="dl">下载</button> <button class="del">删除</button></td>
     </tr>`).join("");
  filesEl.innerHTML = `<table><tr><th>名称</th><th>大小</th><th>模式</th><th>修改时间</th><th></th></tr>
    <tr><td colspan="5"><a href="#" id="parent">.. 上级</a></td></tr>${rows}</table>`;
  filesEl.querySelector("#parent").onclick = (e) => { e.preventDefault();
    listDir(cwd.split("/").slice(0, -1).join("/") || "/"); };
  filesEl.querySelectorAll("tr[data-name]").forEach((tr) => {
    const name = tr.dataset.name, dir = tr.dataset.dir === "true";
    const full = cwd === "/" ? "/" + name : cwd + "/" + name;
    if (dir) tr.ondblclick = () => listDir(full);
    tr.querySelector(".dl").onclick = () => { dl = { name, chunks: [] };
      send(MSG.DOWNLOAD_REQ, JSON.stringify({ path: full })); };
    tr.querySelector(".del").onclick = () => {
      if (confirm("删除 " + full + " ?")) send(MSG.DELETE_REQ, JSON.stringify({ path: full })); };
  });
}
function esc(s) { return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c])); }

document.getElementById("toggle-files").onclick = () => {
  const show = filesEl.style.display !== "block";
  filesEl.style.display = show ? "block" : "none";
  if (show) listDir(cwd === "." ? "." : cwd);
};
document.getElementById("mkdir").onclick = () => {
  const p = prompt("新目录路径:"); if (p) send(MSG.MKDIR_REQ, JSON.stringify({ path: p }));
};
document.getElementById("up").onclick = () => {
  const input = document.createElement("input"); input.type = "file";
  input.onchange = () => uploadFile(input.files[0]);
  input.click();
};
function uploadFile(f) {
  const dest = (cwd === "/" ? "" : cwd) + "/" + f.name;
  send(MSG.UPLOAD_REQ, JSON.stringify({ path: dest, size: f.size }));
  const CHUNK = 32768;
  let off = 0;
  const pump = () => {
    while (off < f.size && ws.bufferedAmount < 4 * CHUNK) {
      const slice = f.slice(off, off + CHUNK);
      slice.arrayBuffer().then((ab) => ws.send(frameRaw(MSG.UPLOAD_DATA, new Uint8Array(ab))));
      off += CHUNK;
    }
    if (off < f.size) setTimeout(pump, 50);
    else ws.send(frame(MSG.UPLOAD_END));
  };
  pump();
}
// drag & drop upload onto file panel
const drop = document.getElementById("drop");
["dragover", "dragenter"].forEach((e) => document.addEventListener(e, (ev) => {
  if (filesEl.style.display === "block") { ev.preventDefault(); drop.classList.add("on"); }
}));
document.addEventListener("dragleave", () => drop.classList.remove("on"));
document.addEventListener("drop", (ev) => {
  if (filesEl.style.display === "block") { ev.preventDefault(); drop.classList.remove("on");
    [...ev.dataTransfer.files].forEach(uploadFile); }
});
```

**Step 5: 跑绿 + 静态资源 200 检查**

```bash
go test -tags webssh ./webssh/ -run 'TestUIServed' -v 2>&1 | tail -3
go build -tags webssh -o /tmp/sshd-lite-webssh.exe . && /tmp/sshd-lite-webssh.exe --webssh --webssh-token demo --webssh-port 18022 &
sleep 2; curl -s -o /dev/null -w "%{http_code} index\n" http://127.0.0.1:18022/ --noproxy "*"; curl -s -o /dev/null -w "%{http_code} xterm\n" http://127.0.0.1:18022/assets/vendor/xterm.js --noproxy "*"; curl -s -o /dev/null -w "%{http_code} appjs\n" http://127.0.0.1:18022/assets/app.js --noproxy "*"
```

预期：test PASS；三个 HTTP 状态均 200。（需要 `Routes()` 里把 `/assets/` 也挂上：`mux.Handle("GET /assets/", http.FileServer(http.FS(assetsFS)))`——加进实现。）

**Step 6: 人工验收（Windows 本机）**

浏览器打开 `http://127.0.0.1:18022/?token=demo`：终端出现、命令可执行、resize 生效、文件面板列目录/上传/下载/删除/新建目录全通。此项人工过一遍后在本 task 提交信息注明 "manually verified in browser on Windows"。

**Step 7: Commit**

```bash
git add webssh/assets.go webssh/assets/ webssh/handler_test.go
git commit -m "feat(webssh): embedded xterm UI with terminal and file panel (no build chain)"
```

---

## Task 8: CLI 集成（TDD）

**Files:**
- Modify: `main.go`（3 处）
- Modify: `webssh_hook.go`（真实实现替换占位）
- Test: `main_test.go`

**Step 1: 写失败测试**

`main_test.go`:
```go
package main

import "testing"

func TestRootBoolFlagIncludesWebssh(t *testing.T) {
	if !rootBoolFlag("--webssh") {
		t.Fatal("--webssh must be a root bool flag or normalizeCLIArgs eats the next token")
	}
}

func TestNormalizeKeepsClientAfterWebssh(t *testing.T) {
	got := normalizeCLIArgs([]string{"sshd-lite", "--webssh", "client"})
	if len(got) < 3 || got[1] != "--webssh" || got[2] != "client" {
		t.Fatalf("got %v", got)
	}
}

func TestStartWebSSHRequiresToken(t *testing.T) {
	err := startWebSSH(t.Context(), "", "", testBaseConfigForHook())
	if err == nil {
		t.Fatal("empty token must error")
	}
}
```

（`testBaseConfigForHook` 返回 `sshd.Config{LogQuiet: true}`；stub 构建下同样报错——但 stub 的错误文案不同，断言只测"非 nil"。注意 `main_test.go` 不带 build tag，两种构建都要编译：`startWebSSH` 两种构建签名一致，OK。）

**Step 2: 跑红**

```bash
go test . -run TestStartWebSSHRequiresToken -v 2>&1 | tail -3
```

预期：编译失败（serverCommand 无 Webssh 字段、startWebSSH 是占位）——占位版 startWebSSH 已存在且 token=="" 时走 env 分支返回 not implemented 错误，`TestStartWebSSHRequiresToken` 可能意外通过。真正的红来自 Step 3 之前 `--webssh` 字段不存在。此处以 `TestRootBoolFlagIncludesWebssh` 为红：`go test . -run TestRootBoolFlag -v` 预期 FAIL。

**Step 3: 实现**

`main.go` 三处修改：

1. `serverCommand`（main.go:55-59）加字段：
```go
type serverCommand struct {
	sshd.Config
	Mosh        bool   `opts:"name=mosh,help=enable Mosh on the same UDP port (five-minute idle timeout)"`
	Webssh      bool   `opts:"name=webssh,help=enable the browser terminal (requires -tags webssh build)"`
	WebsshToken string `opts:"name=webssh-token,help=browser terminal token (or env SSHD_WEBSSH_TOKEN; required with --webssh)"`
	WebsshPort  string `opts:"name=webssh-port,help=browser terminal HTTP port (default 8022)"`
	Command     string `opts:"mode=cmdname"`
}
```

2. `runServer`（main.go:117-140）在 `sshd.NewServer` 之前插入：
```go
	if c.Webssh {
		if err := startWebSSH(ctx, c.WebsshToken, c.WebsshPort, c.Config); err != nil {
			return err
		}
	}
```

3. `rootBoolFlag`（main.go:198-200）case 列表加 `"--webssh"`。

`webssh_hook.go` 替换为：
```go
//go:build webssh

package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jpillora/sshd-lite/sshd"
	"github.com/jpillora/sshd-lite/webssh"
)

// startWebSSH serves the browser terminal UI and WebSocket until ctx is
// cancelled. Empty token falls back to SSHD_WEBSSH_TOKEN, then errors.
func startWebSSH(ctx context.Context, token, port string, base sshd.Config) error {
	if token == "" {
		token = os.Getenv("SSHD_WEBSSH_TOKEN")
	}
	h, err := webssh.New(webssh.Options{Token: token}, base)
	if err != nil {
		return err
	}
	if port == "" {
		port = "8022"
	}
	l, err := net.Listen("tcp", ":"+port)
	if err != nil {
		return errors.New("webssh: " + err.Error())
	}
	srv := &http.Server{Handler: h.Routes()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("webssh: %v", err)
		}
	}()
	log.Printf("webssh: listening on http://0.0.0.0:%s/ (token required)", port)
	return nil
}
```

**Step 4: 跑绿（双 tag）**

```bash
go test . -v 2>&1 | tail -6 && go test -tags webssh . -v 2>&1 | tail -6
```

预期：两种构建下 3 个测试全 PASS。

**Step 5: 端到端冒烟（-tag 构建）**

```bash
go build -tags webssh -o sshd-lite.exe . && (SSHD_WEBSSH_TOKEN= ./sshd-lite.exe --webssh 2>&1 | head -2; echo "exit=$?") ; ./sshd-lite.exe --webssh --webssh-token demo --webssh-port 18023 &
sleep 2 && curl -s -o /dev/null -w "%{http_code} no-token-ws\n" "http://127.0.0.1:18023/webssh" --noproxy "*"
curl -s -o /dev/null -w "%{http_code} ui\n" "http://127.0.0.1:18023/" --noproxy "*"
./sshd-lite.exe 2>&1 --help | grep -c webssh
```

预期：无 token 启动即报错退出（exit=1）；带 token 启动后 `/webssh` 无 token 返回 401、`/` 200；默认构建（`go build -o plain.exe .`）`--help` 无 webssh 字样（flag 在但 stub 报错——两种构建 help 均含 flag，默认构建运行 `--webssh` 报 "requires -tags webssh"，这是预期行为）。

**Step 6: Commit**

```bash
git add main.go main_test.go webssh_hook.go webssh_hook_stub.go
git commit -m "feat(webssh): CLI flags --webssh/--webssh-token/--webssh-port wired to hook"
```

---

## Task 9: 空闲超时与审计（TDD，跨平台）

（handler.go 的 idle watchdog 已在 Task 5 实现——本 task 补测试与审计字段，收口会话日志。）

**Files:**
- Test: `webssh/handler_test.go` 追加

**Step 1: 写失败测试**

```go
func TestIdleTimeoutClosesConnection(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.IdleTimeout = 2 * time.Second })
	c, _, err := wsDial(t, srv.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err = wsRead(ctx, t, c) // blocks until server closes or keep frame
	for err == nil && time.Since(start) < 10*time.Second {
		_, _, err = wsRead(ctx, t, c)
	}
	if err == nil {
		t.Fatal("connection should be closed by idle watchdog")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("closed too late: %v", elapsed)
	}
}
```

（注意：keep ticker 每 30s 才发第一帧，2s idle 窗口内不会干扰；shell 启动耗时计入 idle 起点 `lastAct`——Windows PowerShell 冷启动可能 1-2s，IdleTimeout=2s 偏紧。若 Windows 本机 flaky，改 IdleTimeout=3s 并断言 elapsed > 2s && < 10s。）

**Step 2: 跑红/绿验证**

```bash
go test -tags webssh ./webssh/ -run TestIdleTimeout -v 2>&1 | tail -3
```

预期：PASS（watchdog 已实现）。若 FAIL（连接未关/时序错误）修 handler.go 的 watchdog，禁止改断言迁就实现。审计日志验证：跑测试时观察 stderr 含 `webssh: session open` / `session closed` / （超时用例）`idle timeout`。

**Step 3: Commit**

```bash
git add webssh/handler_test.go webssh/handler.go
git commit -m "test(webssh): idle timeout drops connection; audit open/close/idle logs"
```

---

## Task 10: 收口 —— 全量回归、文档、推送、沙箱实测清单

**Step 1: 双 tag 全量（Windows 本机）**

```bash
go build ./... && go build -tags webssh . && go vet ./... && go vet -tags webssh ./... && go test ./... 2>&1 | tail -8 && go test -tags webssh ./... 2>&1 | tail -8
```

预期：全绿（Windows skip 的 PTY 用例除外）。

**Step 2: WSL 全量（含 PTY 用例）**

```bash
rm -rf ~/sshd-lite && mkdir -p ~/sshd-lite && cd /mnt/e/Go_codes/mole/sshd-lite && tar --exclude=.git -cf - . | (wsl.exe -d Ubuntu-24.04 -- bash -c 'mkdir -p ~/sshd-lite && tar -xf - -C ~/sshd-lite') && wsl.exe -d Ubuntu-24.04 -- bash -ic 'cd ~/sshd-lite && find . -name "*.go" -o -name "*.md" | xargs sed -i "s/\r$//"; go mod tidy && go build -tags webssh . && go test -tags webssh ./webssh/ -v 2>&1 | tail -25' 2>&1 | grep -vE 'job control|nvm|rustup' | tail -30
```

预期：全部用例 PASS（含 TestTerminalEcho、TestWSTerminalRoundtrip）。记录 PASS/FAIL 数。

**Step 3: fork 自述文档**

`docs/webssh.md`（新建，英文，匹配上游仓库语言惯例）：构建（`-tags webssh`）、三个 flag、env、协议帧格式表、安全模型（token 必填、loopback keypair、同源校验、会话上限、空闲超时）、沙箱部署示例（systemd/裸进程 + 反代到唯一子域名出口）。

**Step 4: Commit + push**

```bash
git add docs/webssh.md && git commit -m "docs(webssh): fork module guide (build, flags, protocol, security model)"
git push -u origin feature/webssh
```

**Step 5: 沙箱实测清单（人工，部署后逐项勾选——这是 R1 前提验证）**

1. 上传 `-tags webssh` linux/amd64 二进制到目标沙箱
2. 启动：`SSHD_WEBSSH_TOKEN=<随机> ./sshd-lite --webssh --webssh-port <透明代理后端端口> --quiet`
3. 你的 URL 透明代理指向该端口，浏览器开 `https://af175169279046545.app.workbuddy.host/?token=<token>`
4. 终端：命令执行 / Ctrl+C / vim 交互 / 窗口 resize
5. 文件：列目录 / 上传 >1MB 文件 / 下载 / 删除
6. 稳定性：闲置 10 分钟不断（keep 每 30s 是否穿透代理）；断网重连
7. 记录透明代理的 WS 行为（空闲超时、帧大小限制）到本文档末尾"实测记录"节

任一步失败：透明代理不支持 WS 升级（R1 不成立）→ 回到方案讨论，不改代码硬绕。

---

## 风险与回滚

- 所有改动集中在新增文件 + main.go 三处，rebase upstream 冲突面极小；若上游重构 runServer，hook 调用点跟随移动即可。
- feature/webssh 分支独立，master 始终与上游同步；回滚 = 弃分支。
- 默认构建（无 tag）行为与上游逐字节等价（Task 1/10 双 tag 验证保障）。

---

## 执行记录：计划偏差与修正（2026-10-15 执行时补记）

执行期发现计划中的若干假设与真实代码/环境不符，实际交付以修正后的实现为准。全部偏差如下，均有测试或实测证据：

| # | 计划原文 | 实际修正 | 原因 |
|---|---|---|---|
| D1 | `Dial()` 直接 `ssh.NewClientConn(net.Pipe 客户端半边)` | 客户端半边包一层有界写泵 `pipeWriteBuffer`（64 chunk 队列 + 粘性错误 + 失败关连接） | x/crypto/ssh 握手双方都"先写后读"（version line 与 KEXINIT），net.Pipe 无缓冲 → 双端同时写、无人读 → 死锁至 10s deadline。TCP 内核缓冲掩盖此问题，mosh example 因此未暴露 |
| D2 | `Terminal.Write` 用 `t.sess.Stdin.Write` | `NewTerminal` 里 `sess.StdinPipe()` 存 `stdin io.WriteCloser` 字段 | `ssh.Session.Stdin` 是 `io.Reader`，无 Write 方法（计划代码编译不过） |
| D3 | Task 8 新建 `main_test.go` | **追加**到上游既有的 `main_test.go`（7 个既有测试） | 计划误判该文件不存在；覆盖会删掉上游测试 |
| D4 | 文件路径直接传 SFTP | Windows 增加 `toSFTPPath`：盘符路径 `C:/...` → `/C:/...` | jpillora/sftp 服务端 `toLocalPath` 对非 `/` 开头路径做 `path.Join(workDir, p)`，盘符路径被拼坏；POSIX 行为不变 |
| D5 | handler_test rig 未设读上限 | rig 侧 `SetReadLimit(wsReadLimit)` | coder/websocket 客户端默认读上限 32768B，32KB 数据帧+1B 头超限 |
| D6 | 计划 app.js `handleAck` 判 `ack.mode === "download"` | 改判 `dl && (ack.mode === "read" || ack.name)` | 服务端 download ack 只带 `size`+`name` 不带 `mode`（与 Task 6 实现对齐） |
| D7 | 计划 index.html `<script src="app.js">` | `src="assets/app.js"` | 相对路径在 `/` 下解析为 `/app.js`，无路由命中必 404 |
| D8 | idle watchdog 固定 `time.After(keepInterval)` 轮询 | 轮询间隔取 `min(keepInterval, IdleTimeout/2)` | 固定 30s 轮询使配置的短超时（如 2s）失效，最长延迟 30s；Task 9 测试暴露 |
| D9 | Task 5 `Routes()` 挂 `serveUI` | 推迟到 Task 7 一起挂 | `serveUI` 随 Task 7 的 `assets.go` 出现，Task 5 照抄会编译失败 |

### 未执行/待办项（显式声明）

1. **沙箱实测（计划 Task 10 Step 5）未执行**——需要真实沙箱环境与用户的透明代理，属人工部署环节。清单见计划原文。
2. **IAB 环境下的上传/下载 UI 未点验**——自动化浏览器不支持文件选择器（`capability_unsupported`）；链路本身由 `TestFileOpsRoundtrip`（上传 2 分块 + 下载字节比对）覆盖，且目录创建/删除已在真实浏览器点验并落盘确认。
3. **已知环境噪音**：`sshd/windows` e2e 与 `mosh` 包 UDP 绑定测试在本机 Windows 偶发失败（上游提交同样复现，重跑即过），与本模块无关。
