// Package sftp delivers files to the partner's SFTP server.
//
// Each Upload opens its own SSH connection: the job runs once a day, so a
// pooled connection would only be a stale one. The host key is pinned
// through a known_hosts file, the file is written under a temporary name
// and renamed into place, and missing directories are created on the way.
package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	pkgsftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Options configures a Client.
type Options struct {
	Addr string // host:port
	User string
	// Exactly one of Password and PrivateKeyPEM is used.
	Password      string
	PrivateKeyPEM []byte
	// KnownHostsFile pins the server's host key. Required unless
	// InsecureSkipHostKey is set, which is for local stand-ins only.
	KnownHostsFile      string
	InsecureSkipHostKey bool
	// Timeout bounds the TCP dial and the SSH handshake.
	Timeout time.Duration
}

// Client uploads files to one server.
type Client struct {
	addr    string
	config  *ssh.ClientConfig
	timeout time.Duration
}

// New validates opts and prepares the SSH configuration. It reads the
// known_hosts file now, so a missing file fails at startup, not at 01:00.
func New(opts Options) (*Client, error) {
	if opts.Addr == "" || opts.User == "" {
		return nil, errors.New("sftp: address and user are required")
	}
	var auth []ssh.AuthMethod
	switch {
	case len(opts.PrivateKeyPEM) > 0 && opts.Password != "":
		return nil, errors.New("sftp: use either a password or a private key, not both")
	case len(opts.PrivateKeyPEM) > 0:
		signer, err := ssh.ParsePrivateKey(opts.PrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("sftp: parse private key: %w", err)
		}
		auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	case opts.Password != "":
		auth = []ssh.AuthMethod{ssh.Password(opts.Password)}
	default:
		return nil, errors.New("sftp: a password or a private key is required")
	}

	var hostKey ssh.HostKeyCallback
	switch {
	case opts.InsecureSkipHostKey:
		hostKey = ssh.InsecureIgnoreHostKey() // #nosec G106 -- explicit opt-in for local stand-ins
	case opts.KnownHostsFile != "":
		cb, err := knownhosts.New(opts.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("sftp: load known_hosts: %w", err)
		}
		hostKey = cb
	default:
		return nil, errors.New("sftp: a known_hosts file is required unless host key checks are skipped")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		addr: opts.Addr,
		config: &ssh.ClientConfig{
			User:            opts.User,
			Auth:            auth,
			HostKeyCallback: hostKey,
			Timeout:         timeout,
		},
		timeout: timeout,
	}, nil
}

// HostKeyMismatchError reports a server key that differs from the pinned
// one: either the partner rotated it or the connection is being
// intercepted. Either way the file must not be sent.
type HostKeyMismatchError struct{ Err error }

func (e *HostKeyMismatchError) Error() string {
	return "sftp: host key mismatch: " + e.Err.Error()
}

func (e *HostKeyMismatchError) Unwrap() error { return e.Err }

// Temporary reports false: a retry does not fix a key mismatch.
func (e *HostKeyMismatchError) Temporary() bool { return false }

// Upload writes size bytes from r to remoteDir/name and returns the
// remote path. remoteDir is relative to the login directory and is
// created when missing. The data goes to a temporary name first and is
// renamed when complete, so the partner's poller never picks up a
// half-written file.
func (c *Client) Upload(ctx context.Context, remoteDir, name string, r io.Reader, size int64) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("sftp: file name %q must be a single path element", name)
	}
	if err := validDir(remoteDir); err != nil {
		return "", err
	}
	conn, client, err := c.connect(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	defer client.Close()

	if err := mkdirAll(client, remoteDir); err != nil {
		return "", err
	}
	final := path.Join(remoteDir, name)
	tmp := path.Join(remoteDir, "."+name+".uploading")

	f, err := client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return "", fmt.Errorf("sftp: create %s: %w", tmp, err)
	}
	n, err := io.Copy(f, r)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("wrote %d bytes, expected %d", n, size)
	}
	if err != nil {
		_ = client.Remove(tmp)
		return "", fmt.Errorf("sftp: write %s: %w", tmp, err)
	}

	// Most servers refuse to rename over an existing file; a re-run of the
	// day must replace it, so clear the target first.
	if _, statErr := client.Stat(final); statErr == nil {
		if err := client.Remove(final); err != nil {
			_ = client.Remove(tmp)
			return "", fmt.Errorf("sftp: replace %s: %w", final, err)
		}
	}
	if err := client.PosixRename(tmp, final); err != nil {
		if err := client.Rename(tmp, final); err != nil {
			_ = client.Remove(tmp)
			return "", fmt.Errorf("sftp: publish %s: %w", final, err)
		}
	}
	return final, nil
}

// Stat returns the size of a remote file, or an error when it is missing.
func (c *Client) Stat(ctx context.Context, remotePath string) (int64, error) {
	conn, client, err := c.connect(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	defer client.Close()
	info, err := client.Stat(remotePath)
	if err != nil {
		return 0, fmt.Errorf("sftp: stat %s: %w", remotePath, err)
	}
	return info.Size(), nil
}

// Ping connects and authenticates without transferring anything, for a
// startup check or a readiness probe.
func (c *Client) Ping(ctx context.Context) error {
	conn, client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	_ = client.Close()
	return conn.Close()
}

func (c *Client) connect(ctx context.Context) (*ssh.Client, *pkgsftp.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, nil, fmt.Errorf("sftp: dial %s: %w", c.addr, err)
	}
	// The handshake honours ClientConfig.Timeout; the context covers the dial.
	sshConn, chans, reqs, err := ssh.NewClientConn(raw, c.addr, c.config)
	if err != nil {
		_ = raw.Close()
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			return nil, nil, &HostKeyMismatchError{Err: err}
		}
		return nil, nil, fmt.Errorf("sftp: connect %s: %w", c.addr, err)
	}
	conn := ssh.NewClient(sshConn, chans, reqs)
	client, err := pkgsftp.NewClient(conn)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("sftp: open subsystem: %w", err)
	}
	return conn, client, nil
}

// mkdirAll creates dir and its parents. Servers differ in what Stat
// returns for a missing directory, so every segment is created and an
// "already exists" answer is tolerated.
func mkdirAll(client *pkgsftp.Client, dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	cur := ""
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" {
			continue
		}
		cur = path.Join(cur, seg)
		if info, err := client.Stat(cur); err == nil {
			if !info.IsDir() {
				return fmt.Errorf("sftp: %s exists and is not a directory", cur)
			}
			continue
		}
		if err := client.Mkdir(cur); err != nil {
			if info, statErr := client.Stat(cur); statErr == nil && info.IsDir() {
				continue
			}
			return fmt.Errorf("sftp: mkdir %s: %w", cur, err)
		}
	}
	return nil
}

func validDir(dir string) error {
	if strings.HasPrefix(dir, "/") || strings.Contains(dir, `\`) {
		return fmt.Errorf("sftp: remote dir %q must be relative", dir)
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return fmt.Errorf("sftp: remote dir %q must not contain '..'", dir)
		}
	}
	return nil
}
