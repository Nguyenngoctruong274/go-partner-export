package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
)

// BlobReader opens archived objects.
type BlobReader interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// LinkVerifier checks the proof on a signed link.
type LinkVerifier interface {
	Verify(key, expires, signature string) error
}

// Blob serves signed download links for the fs driver, where no cloud
// provider can serve them. The GCS driver's links point at GCS itself.
type Blob struct {
	Store    BlobReader
	Verifier LinkVerifier
}

func (a *api) routeBlob(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/blob/{key...}", a.downloadBlob)
}

// downloadBlob streams the object once the link's signature and expiry
// check out. Bad links get 403 and expired ones 410, so a dashboard can
// tell "ask for a new link" from "this link was never valid".
func (a *api) downloadBlob(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	q := r.URL.Query()
	if err := a.blob.Verifier.Verify(key, q.Get("expires"), q.Get("signature")); err != nil {
		switch {
		case errors.Is(err, blob.ErrExpired):
			writeError(w, http.StatusGone, "link expired")
		case errors.Is(err, blob.ErrBadSignature):
			writeError(w, http.StatusForbidden, "bad signature")
		default:
			writeError(w, http.StatusBadRequest, "bad key")
		}
		return
	}
	rc, err := a.blob.Store.Open(r.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, "object not found")
		return
	}
	if err != nil {
		a.logger.ErrorContext(r.Context(), "open blob", slog.String("key", key), slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "cannot open object")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", blob.ContentType(key))
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(key)+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
