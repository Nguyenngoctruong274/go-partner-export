package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
)

func newBlobHandler(t *testing.T) (http.Handler, *blob.HMACSigner) {
	t.Helper()
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "\xEF\xBB\xBFA|B\n"
	if err := store.Put(context.Background(), "partner-export/f.csv", "text/csv", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	signer, err := blob.NewHMACSigner("http://export.test", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Blob: &Blob{Store: store, Verifier: signer}})
	return h, signer
}

func TestDownloadBlobWithSignedLink(t *testing.T) {
	h, signer := newBlobHandler(t)
	link, err := signer.SignedURL("partner-export/f.csv", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, link, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="f.csv"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(rec.Body.String(), "\xEF\xBB\xBFA|B") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestDownloadBlobRejects(t *testing.T) {
	h, signer := newBlobHandler(t)
	good, _ := signer.SignedURL("partner-export/f.csv", time.Hour)
	missing, _ := signer.SignedURL("partner-export/missing.csv", time.Hour)
	tests := []struct {
		name string
		url  string
		want int
	}{
		{"no proof", "http://export.test/v1/blob/partner-export/f.csv", http.StatusForbidden},
		{"tampered key", strings.Replace(good, "f.csv", "g.csv", 1), http.StatusForbidden},
		{"tampered signature", good[:len(good)-4] + "0000", http.StatusForbidden},
		{"signed but missing", missing, http.StatusNotFound},
		{"expired", expiredLink(t, signer), http.StatusGone},
		// ".." segments are collapsed by the mux before any handler runs; a
		// backslash is the next trick, and the key validation rejects it.
		{"invalid key", "http://export.test/v1/blob/partner-export/..%5C..%5Cetc?expires=1&signature=00", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.url, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestBlobRoutesAbsentWithoutStore(t *testing.T) {
	h := newTestHandler(nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/blob/x?expires=1&signature=0", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

// expiredLink builds a link whose expiry is in the past, signed correctly.
func expiredLink(t *testing.T, signer *blob.HMACSigner) string {
	t.Helper()
	// SignedURL only accepts positive TTLs, so sign with a tiny one and wait it out.
	link, err := signer.SignedURL("partner-export/f.csv", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// Unix-second granularity: a link that expires at now+1s is dead after
	// up to two seconds.
	time.Sleep(2100 * time.Millisecond)
	return link
}
