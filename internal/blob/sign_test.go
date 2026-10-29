package blob

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)

func TestHMACSignerRoundTrip(t *testing.T) {
	s, err := NewHMACSigner("https://export.internal/", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedNow }

	link, err := s.SignedURL("partner-export/Partner Accrual_v1_20261028_001.csv", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "export.internal" || u.Path != "/v1/blob/partner-export/Partner Accrual_v1_20261028_001.csv" {
		t.Errorf("link = %s", link)
	}
	if strings.Contains(link, " ") {
		t.Errorf("space not escaped in %s", link)
	}
	q := u.Query()
	if q.Get("expires") != "1793239323" { // fixedNow + 1h
		t.Errorf("expires = %s", q.Get("expires"))
	}
	key := strings.TrimPrefix(u.Path, "/v1/blob/")
	if err := s.Verify(key, q.Get("expires"), q.Get("signature")); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err := s.Verify(key, q.Get("expires"), strings.ToUpper(q.Get("signature"))); err != nil {
		t.Errorf("upper-case hex rejected: %v", err)
	}

	// Tampering with the key, the expiry or the signature fails.
	if err := s.Verify("partner-export/other.csv", q.Get("expires"), q.Get("signature")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other key: %v", err)
	}
	if err := s.Verify(key, "9999999999", q.Get("signature")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other expiry: %v", err)
	}
	if err := s.Verify(key, q.Get("expires"), "00"+q.Get("signature")[2:]); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other signature: %v", err)
	}
	if err := s.Verify(key, "soon", q.Get("signature")); !errors.Is(err, ErrBadSignature) {
		t.Errorf("bad expiry: %v", err)
	}
	if err := s.Verify("../x", q.Get("expires"), q.Get("signature")); err == nil {
		t.Error("bad key accepted")
	}

	// After expiry the proof is still right but the link is dead.
	s.now = func() time.Time { return fixedNow.Add(time.Hour + time.Second) }
	if err := s.Verify(key, q.Get("expires"), q.Get("signature")); !errors.Is(err, ErrExpired) {
		t.Errorf("expired: %v", err)
	}

	if _, err := s.SignedURL("../x", time.Hour); err == nil {
		t.Error("bad key signed")
	}
	if _, err := s.SignedURL("k", 0); err == nil {
		t.Error("zero ttl signed")
	}
}

func TestNewHMACSignerRejects(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	for _, base := range []string{"", "export.internal", "ftp://x", "https://"} {
		if _, err := NewHMACSigner(base, secret); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if _, err := NewHMACSigner("http://localhost:8080", []byte("short")); err == nil {
		t.Error("short secret accepted")
	}
}

func testServiceAccount(t *testing.T) (ServiceAccountKey, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	return ServiceAccountKey{ClientEmail: "signer@acme.iam.gserviceaccount.com", PrivateKey: pemText}, key
}

func TestGCSSignerProducesAVerifiableV4URL(t *testing.T) {
	sa, key := testServiceAccount(t)
	s, err := NewGCSSigner(sa, "acme-fin-prod", "")
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return fixedNow }

	link, err := s.SignedURL("partner-export/PartnerAccrual_v1_20261028_001.csv", 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "storage.googleapis.com" ||
		u.Path != "/acme-fin-prod/partner-export/PartnerAccrual_v1_20261028_001.csv" {
		t.Errorf("link = %s", link)
	}
	q := u.Query()
	want := map[string]string{
		"X-Goog-Algorithm":     "GOOG4-RSA-SHA256",
		"X-Goog-Credential":    "signer@acme.iam.gserviceaccount.com/20261029/auto/storage/goog4_request",
		"X-Goog-Date":          "20261029T010203Z",
		"X-Goog-Expires":       "259200",
		"X-Goog-SignedHeaders": "host",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	// Parameters appear sorted and the signature comes last, as in the
	// documented examples.
	if !strings.HasPrefix(u.RawQuery, "X-Goog-Algorithm=") || !strings.Contains(u.RawQuery, "&X-Goog-Signature=") ||
		strings.Contains(u.RawQuery, "+") {
		t.Errorf("raw query = %s", u.RawQuery)
	}
	if strings.Contains(u.RawQuery, "/auto/storage/") || strings.Contains(u.RawQuery, "signer@") {
		t.Errorf("credential scope not escaped: %s", u.RawQuery)
	}

	// Recompute what GCS computes and check the signature with the public key.
	signature, err := hex.DecodeString(q.Get("X-Goog-Signature"))
	if err != nil {
		t.Fatal(err)
	}
	q.Del("X-Goog-Signature")
	canonicalRequest := strings.Join([]string{
		"GET", u.EscapedPath(), canonicalQueryString(q), "host:storage.googleapis.com", "", "host", "UNSIGNED-PAYLOAD",
	}, "\n")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"GOOG4-RSA-SHA256", "20261029T010203Z", "20261029/auto/storage/goog4_request", hex.EncodeToString(sum[:]),
	}, "\n")
	digest := sha256.Sum256([]byte(stringToSign))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	// A different key must not verify.
	_, other := testServiceAccount(t)
	if err := rsa.VerifyPKCS1v15(&other.PublicKey, crypto.SHA256, digest[:], signature); err == nil {
		t.Fatal("signature verified with the wrong key")
	}
}

func TestGCSSignerRejects(t *testing.T) {
	sa, _ := testServiceAccount(t)
	s, err := NewGCSSigner(sa, "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignedURL("k", 8*24*time.Hour); err == nil {
		t.Error("ttl over 7 days accepted")
	}
	if _, err := s.SignedURL("k", 0); err == nil {
		t.Error("zero ttl accepted")
	}
	if _, err := s.SignedURL("/k", time.Hour); err == nil {
		t.Error("bad key accepted")
	}

	if _, err := NewGCSSigner(sa, "", ""); err == nil {
		t.Error("missing bucket accepted")
	}
	if _, err := NewGCSSigner(ServiceAccountKey{PrivateKey: sa.PrivateKey}, "b", ""); err == nil {
		t.Error("missing email accepted")
	}
	if _, err := NewGCSSigner(ServiceAccountKey{ClientEmail: "e", PrivateKey: "not pem"}, "b", ""); err == nil {
		t.Error("bad pem accepted")
	}
	if _, err := NewGCSSigner(sa, "b", "::bad"); err == nil {
		t.Error("bad endpoint accepted")
	}
}

func TestGCSSignerAcceptsPKCS1AndEmulatorHost(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	s, err := NewGCSSigner(ServiceAccountKey{ClientEmail: "e@x", PrivateKey: pkcs1}, "b", "http://localhost:4443")
	if err != nil {
		t.Fatal(err)
	}
	link, err := s.SignedURL("k.csv", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link, "http://localhost:4443/b/k.csv?") {
		t.Errorf("link = %s", link)
	}
}

func TestNewGCSSignerFromFile(t *testing.T) {
	sa, _ := testServiceAccount(t)
	path := filepath.Join(t.TempDir(), "signer.json")
	data, _ := json.Marshal(sa)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGCSSignerFromFile(path, "b", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewGCSSignerFromFile(filepath.Join(t.TempDir(), "missing.json"), "b", ""); err == nil {
		t.Error("missing file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if _, err := NewGCSSignerFromFile(bad, "b", ""); err == nil {
		t.Error("bad json accepted")
	}
}

func TestGCSEscape(t *testing.T) {
	tests := map[string]string{
		"a b": "a%20b", "a/b": "a%2Fb", "e@x": "e%40x", "a=b&c": "a%3Db%26c", "ok-_.~": "ok-_.~",
	}
	for in, want := range tests {
		if got := gcsEscape(in); got != want {
			t.Errorf("gcsEscape(%q) = %q, want %q", in, got, want)
		}
	}
	if got := escapeKey("a b/c d.csv"); got != "a%20b/c%20d.csv" {
		t.Errorf("escapeKey = %q", got)
	}
}

func BenchmarkGCSSignedURL(b *testing.B) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		b.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	sa := ServiceAccountKey{ClientEmail: "s@x.iam.gserviceaccount.com", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))}
	s, err := NewGCSSigner(sa, "bucket", "")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SignedURL("partner-export/PartnerAccrual_v1_20261028_001.csv", time.Hour); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHMACSignedURL(b *testing.B) {
	s, err := NewHMACSigner("https://export.internal", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.SignedURL("partner-export/PartnerAccrual_v1_20261028_001.csv", time.Hour); err != nil {
			b.Fatal(err)
		}
	}
}
