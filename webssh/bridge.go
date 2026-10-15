//go:build webssh

package webssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
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
	return pem.EncodeToMemory(block), signer, nil
}

// Dial opens one loopback SSH connection: the server half of a net.Pipe
// feeds Server.HandleConn in a goroutine (it serves until the connection
// closes); the client half completes a pinned-hostkey handshake.
func (b *Bridge) Dial() (*ssh.Client, error) {
	clientConn, serverConn := net.Pipe()
	go b.server.HandleConn(serverConn)
	conn := newPipeWriteBuffer(clientConn)
	cfg := &ssh.ClientConfig{
		User:            "webssh",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(b.userKey)},
		HostKeyCallback: ssh.FixedHostKey(b.hostKey),
		Timeout:         10 * time.Second,
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, "loopback", cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("webssh: loopback handshake: %w", err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// pipeWriteBuffer decouples Write from the synchronous net.Pipe during the
// SSH handshake. Both peers write before reading: x/crypto/ssh's
// exchangeVersions sends its own version line before reading the peer's,
// and both peers then send KEXINIT before reading. On an unbuffered
// net.Pipe each Write blocks until the peer Reads, so two honest peers
// deadlock until the handshake deadline kills the pipe (a TCP kernel
// buffer hides this on real sockets). Wrapping the client half with a
// bounded background pump breaks the deadlock: buffered writes return
// immediately, so the client reaches its Read while the server's Write
// completes. Once the handshake is done each side runs dedicated read and
// write goroutines, so blocking writes are safe again and the pump is a
// plain pass-through.
type pipeWriteBuffer struct {
	net.Conn            // underlying connection (client half of the pipe)
	ch     chan []byte  // pending writes, bounded
	errCh  chan error   // first background write error (cap 1)
	closed chan struct{}
}

func newPipeWriteBuffer(c net.Conn) *pipeWriteBuffer {
	w := &pipeWriteBuffer{
		Conn:   c,
		ch:     make(chan []byte, 64),
		errCh:  make(chan error, 1),
		closed: make(chan struct{}),
	}
	go w.pump()
	return w
}

func (w *pipeWriteBuffer) pump() {
	failed := false
	for {
		select {
		case buf := <-w.ch:
			if failed {
				continue // drain; error already reported
			}
			if _, err := w.Conn.Write(buf); err != nil {
				failed = true
				select {
				case w.errCh <- err:
				default:
				}
				w.Conn.Close() // unblock the reader side
			}
		case <-w.closed:
			return
		}
	}
}

// Write queues p for the background pump. Order is preserved: callers of
// Write are serialized by the SSH transport (handshake loop or the
// connection's single writer goroutine).
func (w *pipeWriteBuffer) Write(p []byte) (int, error) {
	select {
	case err := <-w.errCh:
		select {
		case w.errCh <- err: // keep for subsequent writes
		default:
		}
		return 0, err
	default:
	}
	buf := make([]byte, len(p))
	copy(buf, p)
	select {
	case w.ch <- buf:
		return len(p), nil
	case <-w.closed:
		return 0, net.ErrClosed
	}
}

func (w *pipeWriteBuffer) Close() error {
	select {
	case <-w.closed:
	default:
		close(w.closed)
	}
	return w.Conn.Close()
}
