package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// DefaultGCSEndpoint is the public Google Cloud Storage API.
const DefaultGCSEndpoint = "https://storage.googleapis.com"

const storageScope = "https://www.googleapis.com/auth/devstorage.read_write"

// GCSOptions configures NewGCS.
type GCSOptions struct {
	Bucket string
	// Endpoint overrides the API base URL, for an emulator such as
	// fake-gcs-server. Empty means DefaultGCSEndpoint.
	Endpoint string
	// CredentialsFile is a service-account JSON key for the client. Empty
	// means Application Default Credentials. Ignored when Endpoint is set:
	// emulators take no authentication.
	CredentialsFile string
	// HTTPClient, when set, is used as is (tests). Otherwise one is built
	// with the credentials.
	HTTPClient *http.Client
}

// GCS stores objects in a Google Cloud Storage bucket through the JSON
// API. It talks HTTP directly, which keeps the dependency small and makes
// every request visible in tests.
type GCS struct {
	bucket   string
	endpoint string
	client   *http.Client
}

// NewGCS returns a store over the bucket in opts.
func NewGCS(ctx context.Context, opts GCSOptions) (*GCS, error) {
	if opts.Bucket == "" {
		return nil, errors.New("blob: gcs bucket is required")
	}
	endpoint := strings.TrimSuffix(opts.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultGCSEndpoint
	}
	client := opts.HTTPClient
	if client == nil {
		var err error
		client, err = gcsHTTPClient(ctx, opts, endpoint)
		if err != nil {
			return nil, err
		}
	}
	return &GCS{bucket: opts.Bucket, endpoint: endpoint, client: client}, nil
}

func gcsHTTPClient(ctx context.Context, opts GCSOptions, endpoint string) (*http.Client, error) {
	if endpoint != DefaultGCSEndpoint {
		return &http.Client{Timeout: 5 * time.Minute}, nil
	}
	var (
		creds *google.Credentials
		err   error
	)
	if opts.CredentialsFile != "" {
		data, readErr := os.ReadFile(opts.CredentialsFile)
		if readErr != nil {
			return nil, fmt.Errorf("blob: read gcs credentials: %w", readErr)
		}
		creds, err = google.CredentialsFromJSON(ctx, data, storageScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, storageScope)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: gcs credentials: %w", err)
	}
	client := oauth2.NewClient(ctx, creds.TokenSource)
	client.Timeout = 5 * time.Minute
	return client, nil
}

// Put uploads the object with a single media upload, which suits files of
// a few megabytes. The write is atomic on the GCS side: the object becomes
// visible only when the upload completes.
func (s *GCS) Put(ctx context.Context, key, contentType string, r io.Reader, size int64) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	q := url.Values{"uploadType": {"media"}, "name": {key}}
	u := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?%s", s.endpoint, url.PathEscape(s.bucket), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return fmt.Errorf("blob: gcs put %s: %w", key, err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", contentType)
	if size >= 0 {
		req.ContentLength = size
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("blob: gcs put %s: %w", key, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("blob: gcs put %s: %w", key, apiError(resp))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Open downloads the object.
func (s *GCS) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/storage/v1/b/%s/o/%s?alt=media", s.endpoint, url.PathEscape(s.bucket), url.PathEscape(key))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("blob: gcs open %s: %w", key, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: gcs open %s: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, fmt.Errorf("blob: gcs open %s: %w", key, apiError(resp))
	}
	return resp.Body, nil
}

// APIError is a non-2xx answer from the storage API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("storage api: http %d: %s", e.Status, e.Body)
}

// Temporary reports whether a retry may succeed: server errors and
// throttling, not client mistakes such as a missing bucket or bad auth.
func (e *APIError) Temporary() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusRequestTimeout
}

func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
}
