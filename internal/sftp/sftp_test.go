package sftp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/sftptest"
)

func passwordClient(t *testing.T, srv *sftptest.Server) *Client {
	t.Helper()
	c, err := New(Options{
		Addr: srv.Addr, User: srv.User, Password: srv.Password,
		KnownHostsFile: srv.WriteKnownHosts(t, t.TempDir(), false), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUploadWithPasswordCreatesDirsAndRenames(t *testing.T) {
	srv := sftptest.Start(t)
	c := passwordClient(t, srv)
	ctx := context.Background()

	body := "\xEF\xBB\xBFA|B\n1|2\n"
	remote, err := c.Upload(ctx, "acme/input", "PartnerAccrual_v1_20261028_001.csv", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if remote != "acme/input/PartnerAccrual_v1_20261028_001.csv" {
		t.Errorf("remote = %q", remote)
	}
	got, err := os.ReadFile(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv"))
	if err != nil || string(got) != body {
		t.Fatalf("file on server: %q, %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Join(srv.Root, "acme", "input"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".uploading") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	if size, err := c.Stat(ctx, remote); err != nil || size != int64(len(body)) {
		t.Errorf("Stat = %d, %v", size, err)
	}

	// A re-run replaces the file; directories already exist.
	body2 := "replaced"
	if _, err := c.Upload(ctx, "acme/input", "PartnerAccrual_v1_20261028_001.csv", strings.NewReader(body2), -1); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv"))
	if string(got) != body2 {
		t.Errorf("after re-run: %q", got)
	}

	// An empty remote dir means the login directory.
	if remote, err := c.Upload(ctx, "", "root.csv", strings.NewReader("x"), 1); err != nil || remote != "root.csv" {
		t.Errorf("upload to login dir: %q, %v", remote, err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestUploadWithPrivateKey(t *testing.T) {
	srv := sftptest.Start(t)
	c, err := New(Options{
		Addr: srv.Addr, User: srv.User, PrivateKeyPEM: []byte(srv.ClientKeyPEM),
		KnownHostsFile: srv.WriteKnownHosts(t, t.TempDir(), false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(context.Background(), "input", "k.csv", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srv.Root, "input", "k.csv")); err != nil {
		t.Fatal(err)
	}
}

func TestHostKeyMismatchIsRefusedAndNotRetried(t *testing.T) {
	srv := sftptest.Start(t)
	c, err := New(Options{
		Addr: srv.Addr, User: srv.User, Password: srv.Password,
		KnownHostsFile: srv.WriteKnownHosts(t, t.TempDir(), true),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Upload(context.Background(), "input", "k.csv", strings.NewReader("x"), 1)
	var mismatch *HostKeyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v, want HostKeyMismatchError", err)
	}
	if mismatch.Temporary() {
		t.Error("mismatch reported as temporary")
	}
	if _, err := os.Stat(filepath.Join(srv.Root, "input", "k.csv")); err == nil {
		t.Error("file was uploaded despite the mismatch")
	}

	// Unknown host (not in the file at all) is refused as well.
	empty := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(empty, nil, 0o600)
	c, err = New(Options{Addr: srv.Addr, User: srv.User, Password: srv.Password, KnownHostsFile: empty})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); !errors.As(err, &mismatch) {
		t.Errorf("unknown host: %v", err)
	}
}

func TestInsecureSkipHostKey(t *testing.T) {
	srv := sftptest.Start(t)
	c, err := New(Options{Addr: srv.Addr, User: srv.User, Password: srv.Password, InsecureSkipHostKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticationFailure(t *testing.T) {
	srv := sftptest.Start(t)
	c, err := New(Options{Addr: srv.Addr, User: srv.User, Password: "wrong", InsecureSkipHostKey: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("err = %v", err)
	}
}

func TestDialFailuresAndContext(t *testing.T) {
	c, err := New(Options{Addr: "127.0.0.1:1", User: "u", Password: "p", InsecureSkipHostKey: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "dial") {
		t.Errorf("closed port: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Upload(ctx, "input", "k.csv", strings.NewReader("x"), 1); err == nil {
		t.Error("cancelled context accepted")
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	kh := filepath.Join(t.TempDir(), "known_hosts")
	_ = os.WriteFile(kh, nil, 0o600)
	bad := []Options{
		{},
		{Addr: "h:22", User: "u", KnownHostsFile: kh},                                             // no auth
		{Addr: "h:22", User: "u", Password: "p"},                                                  // no host key policy
		{Addr: "h:22", User: "u", Password: "p", PrivateKeyPEM: []byte("x"), KnownHostsFile: kh},  // both
		{Addr: "h:22", User: "u", PrivateKeyPEM: []byte("not a key"), KnownHostsFile: kh},         // bad key
		{Addr: "h:22", User: "u", Password: "p", KnownHostsFile: filepath.Join(t.TempDir(), "x")}, // missing file
	}
	for i, o := range bad {
		if _, err := New(o); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestUploadRejectsBadPaths(t *testing.T) {
	srv := sftptest.Start(t)
	c := passwordClient(t, srv)
	ctx := context.Background()
	for _, name := range []string{"", "a/b.csv", `a\b.csv`} {
		if _, err := c.Upload(ctx, "input", name, strings.NewReader("x"), 1); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, dir := range []string{"/abs", "../up", "a/../b", `a\b`} {
		if _, err := c.Upload(ctx, dir, "k.csv", strings.NewReader("x"), 1); err == nil {
			t.Errorf("dir %q accepted", dir)
		}
	}
	// A short body is an error and leaves nothing behind.
	if _, err := c.Upload(ctx, "input", "short.csv", strings.NewReader("abc"), 10); err == nil {
		t.Error("size mismatch accepted")
	}
	if _, err := os.Stat(filepath.Join(srv.Root, "input", "short.csv")); err == nil {
		t.Error("short file published")
	}
	entries, _ := os.ReadDir(filepath.Join(srv.Root, "input"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".uploading") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	// A file where a directory is expected.
	if err := os.WriteFile(filepath.Join(srv.Root, "notadir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, "notadir/input", "k.csv", strings.NewReader("x"), 1); err == nil {
		t.Error("file in the path accepted as a directory")
	}
}
