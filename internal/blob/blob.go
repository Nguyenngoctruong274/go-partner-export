// Package blob archives a copy of every exported file before it is pushed
// to the partner. A Store keeps objects under string keys; the fs driver
// uses a directory and the gcs driver Google Cloud Storage through its
// JSON API, so the pipeline and its tests do not care which one is wired.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Store keeps objects by key.
type Store interface {
	// Put stores size bytes from r under key, replacing an existing object.
	// Drivers write atomically: a reader never sees a partial object.
	Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error
	// Open returns the object's content. The caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// ErrNotFound is returned by Open when the key holds no object.
var ErrNotFound = errors.New("blob: object not found")

// Key joins a prefix and a file name into an object key and rejects
// anything that could escape the prefix. Keys use forward slashes on
// every driver.
func Key(prefix, name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "", fmt.Errorf("blob: file name %q must be a single path element", name)
	}
	if strings.HasPrefix(prefix, "/") || hasDotDot(prefix) || strings.Contains(prefix, `\`) {
		return "", fmt.Errorf("blob: prefix %q must be relative without '..'", prefix)
	}
	key := path.Join(prefix, name)
	if key == "." || key == "" {
		return "", fmt.Errorf("blob: empty key from prefix %q and name %q", prefix, name)
	}
	return key, nil
}

// ValidKey reports whether key could have come from Key.
func ValidKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		hasDotDot(key) || strings.Contains(key, `\`) || strings.Contains(key, "//") {
		return fmt.Errorf("blob: invalid key %q", key)
	}
	return nil
}

func hasDotDot(p string) bool {
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return true
		}
	}
	return false
}

// ContentType picks a media type from the file extension; the archive is
// browsed by people, and the right type lets a browser open a CSV as text.
func ContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	default:
		return "application/octet-stream"
	}
}
