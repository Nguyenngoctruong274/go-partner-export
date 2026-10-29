// Package sftptest runs an SSH server with the SFTP subsystem in-process,
// so the client and the pipeline are tested against a real protocol
// exchange without Docker.
package sftptest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Server is a running test server.
type Server struct {
	// Addr is host:port to dial.
	Addr string
	// Root is the directory the server serves. Relative remote paths
	// resolve against it, as a chrooted partner account would.
	Root string
	// KnownHosts is the content of a known_hosts file pinning this
	// server's host key.
	KnownHosts string
	// WrongKnownHosts pins a different key, for negative tests.
	WrongKnownHosts string
	// User and Password are the accepted credentials; AuthorizedKey is the
	// client private key (PEM) whose public half is accepted.
	User, Password string
	ClientKeyPEM   string

	ln net.Listener
}

// Start runs a server until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	root := t.TempDir()

	hostSigner, _ := newSigner(t)
	wrongSigner, _ := newSigner(t)
	clientSigner, clientPEM := newSigner(t)
	clientPub := clientSigner.PublicKey().Marshal()

	const user, password = "partner", "partner-secret"
	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if conn.User() == user && string(pw) == password {
				return &ssh.Permissions{}, nil
			}
			return nil, errAuth
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if conn.User() == user && string(key.Marshal()) == string(clientPub) {
				return &ssh.Permissions{}, nil
			}
			return nil, errAuth
		},
	}
	config.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("sftptest: listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go serve(ln, config, root)

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	line := func(s ssh.Signer) string {
		return "[" + host + "]:" + port + " " + string(ssh.MarshalAuthorizedKey(s.PublicKey()))
	}
	return &Server{
		Addr: ln.Addr().String(), Root: root,
		KnownHosts: line(hostSigner), WrongKnownHosts: line(wrongSigner),
		User: user, Password: password, ClientKeyPEM: clientPEM,
		ln: ln,
	}
}

// WriteKnownHosts writes the pinning file into dir and returns its path.
func (s *Server) WriteKnownHosts(t testing.TB, dir string, wrong bool) string {
	t.Helper()
	content := s.KnownHosts
	if wrong {
		content = s.WrongKnownHosts
	}
	path := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// WriteClientKey writes the accepted client private key into dir.
func (s *Server) WriteClientKey(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, []byte(s.ClientKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Close stops accepting connections.
func (s *Server) Close() { _ = s.ln.Close() }

type authError struct{}

func (authError) Error() string { return "sftptest: authentication failed" }

var errAuth = authError{}

func newSigner(t testing.TB) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemText := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	signer, err := ssh.ParsePrivateKey(pemText)
	if err != nil {
		t.Fatal(err)
	}
	return signer, string(pemText)
}

func serve(ln net.Listener, config *ssh.ServerConfig, root string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			sshConn, chans, reqs, err := ssh.NewServerConn(c, config)
			if err != nil {
				return
			}
			defer sshConn.Close()
			go ssh.DiscardRequests(reqs)
			for ch := range chans {
				if ch.ChannelType() != "session" {
					_ = ch.Reject(ssh.UnknownChannelType, "unknown channel type")
					continue
				}
				channel, requests, err := ch.Accept()
				if err != nil {
					continue
				}
				go handleSession(channel, requests, root)
			}
		}(conn)
	}
}

func handleSession(channel ssh.Channel, requests <-chan *ssh.Request, root string) {
	for req := range requests {
		if req.Type != "subsystem" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Subsystem string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil || payload.Subsystem != "sftp" {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		// Relative remote paths resolve against root, as they would in a
		// partner account chrooted to its home directory.
		server, err := sftp.NewServer(channel, sftp.WithServerWorkingDirectory(root))
		if err != nil {
			return
		}
		_ = server.Serve()
		_ = server.Close()
		return
	}
}
