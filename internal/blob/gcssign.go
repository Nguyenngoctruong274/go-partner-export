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
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GCS V4 signing constants, see
// https://cloud.google.com/storage/docs/access-control/signing-urls-manually.
const (
	gcsAlgorithm  = "GOOG4-RSA-SHA256"
	gcsScopeTail  = "auto/storage/goog4_request"
	gcsDateLayout = "20060102T150405Z"
	gcsDayLayout  = "20060102"
	gcsMaxTTL     = 7 * 24 * time.Hour
)

// ServiceAccountKey is the part of a Google service-account JSON key file
// that signing needs. Workload Identity and metadata-server credentials
// carry no private key, which is why signing takes a key file of its own.
type ServiceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
}

// GCSSigner signs V4 URLs for objects in one bucket with a service-account
// RSA key. It needs no network: the signature is computed locally and
// verified by GCS with the account's public key.
type GCSSigner struct {
	bucket string
	host   string
	scheme string
	email  string
	key    *rsa.PrivateKey
	now    func() time.Time
}

// NewGCSSignerFromFile reads a service-account JSON key file.
func NewGCSSignerFromFile(path, bucket, endpoint string) (*GCSSigner, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from configuration
	if err != nil {
		return nil, fmt.Errorf("blob: read signer key: %w", err)
	}
	var sa ServiceAccountKey
	if err := json.Unmarshal(data, &sa); err != nil {
		return nil, fmt.Errorf("blob: parse signer key file: %w", err)
	}
	return NewGCSSigner(sa, bucket, endpoint)
}

// NewGCSSigner builds a signer from a parsed key. endpoint is the storage
// host links point at; empty means DefaultGCSEndpoint.
func NewGCSSigner(sa ServiceAccountKey, bucket, endpoint string) (*GCSSigner, error) {
	if bucket == "" {
		return nil, errors.New("blob: gcs bucket is required")
	}
	if sa.ClientEmail == "" {
		return nil, errors.New("blob: signer key has no client_email")
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		endpoint = DefaultGCSEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("blob: bad endpoint %q", endpoint)
	}
	return &GCSSigner{bucket: bucket, host: u.Host, scheme: u.Scheme, email: sa.ClientEmail, key: key, now: time.Now}, nil
}

func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("blob: signer key has no PEM private_key")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("blob: signer private_key is not RSA")
		}
		return rsaKey, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("blob: signer private_key is neither PKCS#8 nor PKCS#1")
}

// SignedURL implements Signer with the V4 algorithm:
//
//  1. Build the canonical request: method, path, sorted query (the
//     X-Goog-* parameters except the signature), signed headers (host),
//     and UNSIGNED-PAYLOAD.
//  2. Build the string to sign: algorithm, timestamp, credential scope
//     and the SHA-256 of the canonical request.
//  3. Sign it with RSA PKCS#1 v1.5 over SHA-256 and hex-encode.
//  4. Append X-Goog-Signature to the query.
func (s *GCSSigner) SignedURL(key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 || ttl > gcsMaxTTL {
		return "", fmt.Errorf("blob: ttl must be within (0, %s]", gcsMaxTTL)
	}
	now := s.now().UTC()
	scope := now.Format(gcsDayLayout) + "/" + gcsScopeTail
	path := "/" + escapeKey(s.bucket) + "/" + escapeKey(key)

	query := url.Values{
		"X-Goog-Algorithm":     {gcsAlgorithm},
		"X-Goog-Credential":    {s.email + "/" + scope},
		"X-Goog-Date":          {now.Format(gcsDateLayout)},
		"X-Goog-Expires":       {strconv.FormatInt(int64(ttl/time.Second), 10)},
		"X-Goog-SignedHeaders": {"host"},
	}
	canonicalQuery := canonicalQueryString(query)

	canonicalRequest := strings.Join([]string{
		"GET",
		path,
		canonicalQuery,
		"host:" + s.host,
		"", // end of headers
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")
	sum := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		gcsAlgorithm,
		now.Format(gcsDateLayout),
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")

	digest := sha256.Sum256([]byte(stringToSign))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("blob: sign: %w", err)
	}
	return s.scheme + "://" + s.host + path + "?" + canonicalQuery + "&X-Goog-Signature=" + hex.EncodeToString(sig), nil
}

// canonicalQueryString sorts parameters by name and percent-encodes them
// the way GCS expects (RFC 3986: spaces as %20, '/' and ':' escaped).
func canonicalQueryString(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, gcsEscape(k)+"="+gcsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func gcsEscape(s string) string {
	// url.QueryEscape encodes everything but unreserved characters, as GCS
	// wants, except that it writes a space as '+', which GCS rejects.
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}
