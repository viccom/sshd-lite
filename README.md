# sshd-lite

[![GoDoc](https://img.shields.io/static/v1?label=godoc&message=reference&color=00add8)](https://pkg.go.dev/github.com/jpillora/sshd-lite)
[![CI](https://github.com/jpillora/sshd-lite/workflows/CI/badge.svg)](https://github.com/jpillora/sshd-lite/actions?workflow=CI)

A lightweight, cross-platform SSH daemon written in Go. It runs shells and
commands as the sshd-lite process user, without operating-system user lookup or
privilege switching.

## Install

Download the latest binary from [GitHub Releases](https://github.com/jpillora/sshd-lite/releases/latest),
use the installer, or install from source:

```sh
curl https://i.jpillora.com/sshd-lite! | bash
```

Or install from source:

```sh
go install github.com/jpillora/sshd-lite@latest
```

## Features

- Interactive shells and remote command execution
- Password, public-key, GitHub-key, or no authentication
- SFTP, with optional confinement to the configured work directory
- Local and reverse TCP forwarding
- Built-in SSH client with known-host verification
- Mosh-compatible terminal sessions over UDP
- Browser terminal + SFTP over a single WebSocket (`webssh` build tag, added by this fork)
- Ed25519 and RSA server keys
- Linux, macOS, and Windows binaries with no runtime dependencies

## Quick start

Start a password-authenticated server:

```sh
sshd-lite --port 2222 user:pass
ssh -p 2222 user@localhost
```

Enable SFTP and restrict it to the work directory:

```sh
sshd-lite --port 2222 --workdir /srv/ssh --sftp --sftp-wd user:pass
```

Use the built-in client for a shell or remote command:

```sh
sshd-lite client --port 2222 user@host
sshd-lite client --port 2222 user@host 'uname -a'
```

Invoking the binary through a symlink named `ssh-lite` selects client mode
automatically. The client trusts new hosts into its private XDG state file and
still warns on changed keys. Use `--strict-hosts` to confirm new keys,
`--share-known-hosts` to use `~/.ssh/known_hosts`, or `--insecure` only to
disable verification.

Enable Mosh on the server and client, with both TCP and UDP open on the same
port:

```sh
sshd-lite --mosh --port 2222 user:pass
sshd-lite client --mosh --port 2222 user@host
```

See the [Mosh guide](docs/mosh-guide.md) for setup, interoperability, session
lifetime, terminal behavior, limitations, and Go API usage.

## Browser terminal (webssh)

> Added by this fork; not present in upstream `jpillora/sshd-lite`. This
> fork's tag releases ship binaries and Docker images built with the module
> enabled; the upstream installer above does not — build from source with
> the build tag.

A build-tag-gated module that turns sshd-lite into a browser-reachable SSH
endpoint: an embedded xterm.js UI speaks one WebSocket to an in-process sshd
bridge, so the process needs no OS sshd and no inbound port other than its
own HTTP listener — built for sandboxes whose only ingress is an HTTPS
subdomain behind a URL proxy. The UI provides an interactive shell plus an
SFTP file panel (upload/download with progress, directory management, and
markdown/source-code preview).

Build and run:

```sh
go build -tags webssh -o sshd-lite-webssh .
./sshd-lite-webssh --webssh --webssh-token <token> --webssh-port 8022 none
```

Then open `http://<host>:8022/?token=<token>`. Without the tag the flags
exist but refuse to start, and no new dependency is linked into the default
build. `sshd-lite --version` reports the release identity as
`<git tag>-<git hash>-<build date>`. Tokens are constant-time compared and
required; sessions are capped (default 4) and dropped after an idle timeout
(default 4h). Behind a reverse proxy that rewrites the `Host` header, pass
`--webssh-origin` with a glob such as `*.example.com` to accept the proxy's
browser origin. The wire protocol is compatible with the mole project's
webssh tunnel.

See the [webssh module guide](docs/webssh.md) for the protocol table,
security model, sandbox deployment, and verification matrix.

## Configuration summary

- The authentication argument may be `user:pass`, an unrestricted
  `authorized_keys` path, `github.com/user`, or `none`.
- Shells, commands, and SFTP start in `--workdir`. `--sftp-wd` additionally
  makes that directory SFTP's virtual root.
- Sessions inherit the process environment by default. Use `--no-inherit-env`,
  `--no-global-env`, or `--no-client-env` to tighten environment handling.
- SFTP and TCP forwarding are disabled unless explicitly enabled.
- `MAX_PENDING_HANDSHAKES` sets the CLI default for concurrent unauthenticated
  handshakes.

Run `sshd-lite --help` or `sshd-lite client --help` for the complete CLI
reference.

## Go packages

The public [`sshd`](https://pkg.go.dev/github.com/jpillora/sshd-lite/sshd)
package configures and runs servers. [`client`](https://pkg.go.dev/github.com/jpillora/sshd-lite/client)
provides the built-in SSH client behavior, [`mosh`](https://pkg.go.dev/github.com/jpillora/sshd-lite/mosh)
is the opt-in Mosh API, and [`xssh`](https://pkg.go.dev/github.com/jpillora/sshd-lite/xssh)
exposes lower-level SSH connection and handler APIs.

See the [architecture guide](docs/architecture.md) for package ownership,
dependency boundaries, listener lifecycle, and extension points.

## Documentation

- [Mosh guide](docs/mosh-guide.md)
- [Mosh compatibility and verification](docs/mosh-compatibility.md)
- [webssh module guide](docs/webssh.md) (fork addition)
- [Architecture](docs/architecture.md)
- [In-process Mosh example](example/mosh/main.go)

## License

[MIT](LICENSE) © 2026 Jaime Pillora
