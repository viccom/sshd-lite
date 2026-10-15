# webssh (fork addition)

A build-tag-gated browser terminal for sshd-lite: SSH shell plus SFTP file
management over one WebSocket. Added by this fork; the default build is
byte-for-byte upstream behavior.

## Why

Cloud sandboxes often have no sshd and no way to expose a TCP port, but do
provide an HTTPS subdomain as their only ingress. This module turns
sshd-lite itself into a browser-reachable SSH endpoint: the browser talks
WebSocket to a process-internal sshd, so nothing needs to listen on port 22
and no external relay server is involved.

```
browser (xterm.js)
  ⇄ WebSocket  /webssh?token=...
webssh module (HTTP handler, message dispatch)
  ⇄ net.Pipe (in-process)
sshd.Server.HandleConn  ⇄  ssh.NewClientConn with a one-time ed25519 key
  ⇄ this process's own PTY shell / SFTP subsystem
```

## Build

```sh
go build -tags webssh -o sshd-lite-webssh .
```

Without the tag the flag exists but errors with
`--webssh requires a build with -tags webssh`; no new dependency is linked.

## Run

```sh
./sshd-lite-webssh --webssh --webssh-token <token> --webssh-port 8022 <auth>
```

| flag | meaning |
|---|---|
| `--webssh` | enable the browser terminal |
| `--webssh-token` | access token (falls back to env `SSHD_WEBSSH_TOKEN`); **required** — startup fails without it |
| `--webssh-port` | HTTP listen port (default `8022`) |

Then open `http://<host>:8022/?token=<token>`. The token is also accepted as
a `?token=` query parameter on the WebSocket itself, which is what the page
uses; `localStorage` caches it after the first prompt.

The ordinary `<auth>` argument still governs the normal SSH listener; the
webssh shell does not use it. The internal bridge generates its own one-time
keypair and injects the public half through `sshd.Config.AuthKeys`.

## Protocol

One WebSocket binary message is `[type 1B][payload]`; payloads are raw bytes
for data frames and UTF-8 JSON for file operations. This is wire-compatible
with mole's webssh tunnel protocol, so a mole admin UI could drive this
endpoint directly.

| type | direction | payload |
|---|---|---|
| `0x01` Data | both | terminal bytes |
| `0x02` Resize | browser → server | `{"cols":int,"rows":int}` |
| `0x03` Keep | both | none (receiver ignores) |
| `0x04` FileListReq | browser → server | `{"path":string}` |
| `0x05` FileUploadReq | browser → server | `{"path":string,"size":int}` |
| `0x06` FileUploadData | browser → server | raw chunk, ≤32 KiB each |
| `0x07` FileUploadEnd | browser → server | none |
| `0x08` FileDownloadReq | browser → server | `{"path":string}` |
| `0x09` FileDeleteReq | browser → server | `{"path":string}` |
| `0x0A` FileMkdirReq | browser → server | `{"path":string}` |
| `0x0B` FileReadReq | browser → server | `{"path":string}` (ack carries `mode:"read"`) |
| `0x0C` FileListResp | server → browser | `{"path","home","files":[{name,size,mode,mod_time,is_dir}],"truncated"?}` |
| `0x0D` FileDataResp | server → browser | raw chunk (download/read stream) |
| `0x0E` FileAckResp | server → browser | `{"ok","msg","size"?,"name"?,"mode"?}` |
| `0x0F` FileReadResp | server → browser | reserved, kept for mole parity |

Directory listings larger than 65535 bytes are truncated to 3/4 of the
entries repeatedly and flagged `truncated`.

## Security model

- **The token is the only gate.** A browser that presents it gets a shell
  running as this process. There is no privilege separation (same model as
  sshd-lite itself).
- The loopback SSH keypair is generated at startup and lives only in memory;
  it is never written to disk and never crosses the network.
- The WebSocket upgrade uses same-origin checks; non-browser clients without
  an `Origin` header are allowed, matching mole's behavior.
- Token comparison is constant-time.
- Defaults: 4 concurrent sessions (`429` beyond that), 4h idle timeout,
  64 KiB WS read limit, 30s server keepalive.

## Sandbox deployment

```sh
# instance-local random token, kept out of logs
export SSHD_WEBSSH_TOKEN=$(head -c 24 /dev/urandom | base64)
./sshd-lite-webssh --webssh --webssh-port 8022 none --quiet &
```

Point the sandbox's HTTPS subdomain at the webssh port and open
`https://<subdomain>/?token=<token>`. Note the token will appear in the
ingress proxy's access log when passed as a query parameter; rotate it per
instance.

## Test matrix

| area | Windows | WSL (Ubuntu 24.04) |
|---|---|---|
| frame codec, bridge, SFTP, auth, session cap | pass | pass |
| PTY terminal (echo, resize, teardown) | skip (needs PTY) | pass |
| WS terminal roundtrip, idle timeout | skip (needs PTY) | pass |

Known environment noise on this host, reproducible on the unmodified
upstream commit: `sshd/windows` e2e (temp-key ACL) and UDP-bind tests in the
`mosh` package can fail intermittently on Windows when the OS reserved-port
range collides; they pass on retry.

## Verified end-to-end (browser acceptance)

Executed against a real browser on Windows: terminal command echo, file
panel directory listing, file delete, and directory creation (each confirmed
on disk). Download uses a blob anchor; the transfer path is covered by
`TestFileOpsRoundtrip` in the test suite.
