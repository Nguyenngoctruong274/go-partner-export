package blob

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Signer produces a time-limited download link for an archived object.
// The link carries its own proof, so whoever holds it can download the
// object until it expires without any credential of their own.
type Signer interface {
	SignedURL(key string, ttl time.Duration) (string, error)
}

// ErrBadSignature is returned when a link's proof does not verify.
var ErrBadSignature = errors.New("blob: bad signature")

// ErrExpired is returned when a link's validity has passed.
var ErrExpired = errors.New("blob: link expired")

// HMACSigner signs links served by exportd itself for the fs driver:
// <base>/v1/blob/<key>?expires=<unix>&signature=<hex HMAC-SHA256>. The
// HMAC covers the key and the expiry, so neither can be changed without
// the secret.
type HMACSigner struct {
	base   string
	secret []byte
	now    func() time.Time
}

// NewHMACSigner returns a signer for links under base (scheme and host of
// the service, e.g. https://export.internal). The secret must stay the
// same across restarts and instances, or links stop verifying.
func NewHMACSigner(base string, secret []byte) (*HMACSigner, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("blob: base URL %q must be http(s) with a host", base)
	}
	if len(secret) < 16 {
		return nil, errors.New("blob: hmac secret must be at least 16 bytes")
	}
	return &HMACSigner{base: strings.TrimSuffix(base, "/"), secret: secret, now: time.Now}, nil
}

// SignedURL implements Signer.
func (s *HMACSigner) SignedURL(key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		return "", errors.New("blob: ttl must be positive")
	}
	expires := s.now().Add(ttl).Unix()
	q := url.Values{
		"expires":   {strconv.FormatInt(expires, 10)},
		"signature": {s.sign(key, expires)},
	}
	return s.base + "/v1/blob/" + escapeKey(key) + "?" + q.Encode(), nil
}

// Verify checks the proof carried by a request for key.
func (s *HMACSigner) Verify(key, expires, signature string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	exp, err := strconv.ParseInt(expires, 10, 64)
	if err != nil {
		return ErrBadSignature
	}
	want := s.sign(key, exp)
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(signature))) {
		return ErrBadSignature
	}
	if s.now().Unix() > exp {
		return ErrExpired
	}
	return nil
}

func (s *HMACSigner) sign(key string, expires int64) string {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = fmt.Fprintf(mac, "%s\n%d", key, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// escapeKey escapes each path element but keeps the slashes.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}
