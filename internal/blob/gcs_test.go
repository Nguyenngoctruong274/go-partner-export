package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// TestGCSEmulatorEnv names the variable pointing at a fake-gcs-server.
const TestGCSEmulatorEnv = "PX_TEST_GCS_ENDPOINT"

// fakeAPI is the slice of the JSON API the driver uses, in memory.
type fakeAPI struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	fail    int // next N requests answer 503
	puts    int
}

func (f *fakeAPI) handler(t *testing.T, bucket string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail > 0 {
			f.fail--
			http.Error(w, `{"error":"backend"}`, http.StatusServiceUnavailable)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/"+bucket+"/o":
			if r.URL.Query().Get("uploadType") != "media" {
				http.Error(w, "uploadType", http.StatusBadRequest)
				return
			}
			name := r.URL.Query().Get("name")
			body, _ := io.ReadAll(r.Body)
			if r.ContentLength >= 0 && int(r.ContentLength) != len(body) {
				t.Errorf("Content-Length %d, body %d", r.ContentLength, len(body))
			}
			f.objects[name] = body
			f.types[name] = r.Header.Get("Content-Type")
			f.puts++
			_, _ = w.Write([]byte(`{"name":"` + name + `"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/storage/v1/b/"+bucket+"/o/"):
			name, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/storage/v1/b/"+bucket+"/o/"))
			if err != nil || r.URL.Query().Get("alt") != "media" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			body, ok := f.objects[name]
			if !ok {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", f.types[name])
			_, _ = w.Write(body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	})
}

func newFakeGCS(t *testing.T) (*GCS, *fakeAPI) {
	t.Helper()
	api := &fakeAPI{objects: map[string][]byte{}, types: map[string]string{}}
	srv := httptest.NewServer(api.handler(t, "fin"))
	t.Cleanup(srv.Close)
	store, err := NewGCS(context.Background(), GCSOptions{Bucket: "fin", Endpoint: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	return store, api
}

func TestGCSStoreAgainstFakeAPI(t *testing.T) {
	store, api := newFakeGCS(t)
	storeSuite(t, store)
	if api.types["partner-export/f.csv"] != "text/csv" {
		t.Errorf("content type = %q", api.types["partner-export/f.csv"])
	}
}

func TestGCSErrors(t *testing.T) {
	store, api := newFakeGCS(t)
	ctx := context.Background()

	api.fail = 1
	err := store.Put(ctx, "k.csv", "text/csv", strings.NewReader("x"), 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || !apiErr.Temporary() {
		t.Fatalf("Put during outage = %v", err)
	}
	if !strings.Contains(err.Error(), "http 503") {
		t.Errorf("error text: %v", err)
	}

	api.fail = 1
	if _, err := store.Open(ctx, "k.csv"); !errors.As(err, &apiErr) {
		t.Fatalf("Open during outage = %v", err)
	}

	for _, e := range []APIError{{Status: 400}, {Status: 403}, {Status: 404}} {
		if e.Temporary() {
			t.Errorf("%d reported temporary", e.Status)
		}
	}
	for _, e := range []APIError{{Status: 500}, {Status: 429}, {Status: 408}} {
		if !e.Temporary() {
			t.Errorf("%d reported permanent", e.Status)
		}
	}

	if _, err := NewGCS(ctx, GCSOptions{}); err == nil {
		t.Error("missing bucket accepted")
	}
	if _, err := NewGCS(ctx, GCSOptions{Bucket: "b", CredentialsFile: "/nonexistent/key.json"}); err == nil {
		t.Error("missing credentials file accepted")
	}
}

func TestGCSDefaultsToTheRealEndpointWithTimeout(t *testing.T) {
	store, err := NewGCS(context.Background(), GCSOptions{Bucket: "b", HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if store.endpoint != DefaultGCSEndpoint {
		t.Errorf("endpoint = %q", store.endpoint)
	}
}

// TestGCSStoreAgainstEmulator runs the suite against fake-gcs-server when
// PX_TEST_GCS_ENDPOINT is set (docker compose exposes it on :4443).
func TestGCSStoreAgainstEmulator(t *testing.T) {
	endpoint := os.Getenv(TestGCSEmulatorEnv)
	if endpoint == "" {
		t.Skip(TestGCSEmulatorEnv + " not set; skipping emulator test")
	}
	ctx := context.Background()
	bucket := "test-" + strings.ToLower(t.Name())
	// The emulator creates buckets through the JSON API; real GCS would
	// need a project and permissions, which is why this stays in tests.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+"/storage/v1/b",
		bytes.NewReader([]byte(`{"name":"`+bucket+`"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		t.Fatalf("create bucket: http %d", resp.StatusCode)
	}
	store, err := NewGCS(ctx, GCSOptions{Bucket: bucket, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	storeSuite(t, store)
}
