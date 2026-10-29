package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKey(t *testing.T) {
	tests := []struct {
		prefix, name string
		want         string
		wantErr      bool
	}{
		{"partner-export", "a.csv", "partner-export/a.csv", false},
		{"", "a.csv", "a.csv", false},
		{"x/y", "a.csv", "x/y/a.csv", false},
		{"x/", "a.csv", "x/a.csv", false},
		{"partner-export", "", "", true},
		{"partner-export", "dir/a.csv", "", true},
		{"partner-export", `..\a.csv`, "", true},
		{"/abs", "a.csv", "", true},
		{"../up", "a.csv", "", true},
		{"a/../b", "a.csv", "", true},
	}
	for _, tt := range tests {
		got, err := Key(tt.prefix, tt.name)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("Key(%q, %q) = %q, %v; want %q, err=%v", tt.prefix, tt.name, got, err, tt.want, tt.wantErr)
		}
		if err == nil {
			if err := ValidKey(got); err != nil {
				t.Errorf("ValidKey(%q) = %v", got, err)
			}
		}
	}
	for _, bad := range []string{"", "/a", "a/", "a//b", "a/../b", `a\b`} {
		if ValidKey(bad) == nil {
			t.Errorf("ValidKey(%q) accepted", bad)
		}
	}
}

func TestContentType(t *testing.T) {
	tests := map[string]string{
		"a.csv": "text/csv; charset=utf-8", "A.CSV": "text/csv; charset=utf-8",
		"a.txt": "text/plain; charset=utf-8", "a.json": "application/json",
		"a.pdf": "application/pdf", "a.zip": "application/zip",
		"a.xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"a.bin":  "application/octet-stream", "noext": "application/octet-stream",
	}
	for name, want := range tests {
		if got := ContentType(name); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

// storeSuite runs the behaviour every driver must share.
func storeSuite(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()

	if _, err := store.Open(ctx, "partner-export/missing.csv"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing = %v, want ErrNotFound", err)
	}

	body := []byte("\xEF\xBB\xBFA|B\n1|2\n")
	if err := store.Put(ctx, "partner-export/f.csv", "text/csv", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	rc, err := store.Open(ctx, "partner-export/f.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %q, %v", got, err)
	}

	// Put replaces.
	body2 := []byte("replaced")
	if err := store.Put(ctx, "partner-export/f.csv", "text/csv", bytes.NewReader(body2), -1); err != nil {
		t.Fatal(err)
	}
	rc, err = store.Open(ctx, "partner-export/f.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, body2) {
		t.Fatalf("after replace: %q", got)
	}

	for _, bad := range []string{"", "/x", "../x", "a/../b"} {
		if err := store.Put(ctx, bad, "", strings.NewReader("x"), 1); err == nil {
			t.Errorf("Put(%q) accepted", bad)
		}
		if _, err := store.Open(ctx, bad); err == nil {
			t.Errorf("Open(%q) accepted", bad)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.Put(cancelled, "partner-export/c.csv", "", strings.NewReader("x"), 1); err == nil {
		t.Error("Put with cancelled context accepted")
	}
}

func TestFSStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blob")
	store, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	if store.Root() != root && !strings.HasSuffix(store.Root(), "blob") {
		t.Errorf("Root = %q", store.Root())
	}
	storeSuite(t, store)

	// No temporary files are left behind, and the object has a sane mode.
	entries, err := os.ReadDir(filepath.Join(root, "partner-export"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	info, err := os.Stat(filepath.Join(root, "partner-export", "f.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}

	// A short read is an error and leaves the previous object intact.
	err = store.Put(context.Background(), "partner-export/f.csv", "", strings.NewReader("abc"), 10)
	if err == nil {
		t.Fatal("size mismatch accepted")
	}
	rc, err := store.Open(context.Background(), "partner-export/f.csv")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "replaced" {
		t.Errorf("object changed by a failed put: %q", got)
	}

	if _, err := NewFS(""); err == nil {
		t.Error("empty root accepted")
	}
}
