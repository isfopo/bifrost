// Package auth verifies incoming webhook requests according to the
// from-service's configured authentication strategy.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"bifrost/internal/config"
)

// Verify checks an incoming webhook request against the from-service's
// configured authentication. body must be the raw request body.
func Verify(a config.FromAuth, r *http.Request, body []byte) error {
	switch a.Type {
	case "none", "":
		return nil
	case "header":
		got := r.Header.Get(a.Header)
		if got == "" {
			return config.ErrAuth
		}
		if !hmac.Equal([]byte(got), []byte(a.Secret())) {
			return config.ErrAuth
		}
		return nil
	case "hmac":
		got := r.Header.Get(a.Header)
		if got == "" {
			return config.ErrAuth
		}
		got = trimPrefix(got, a.Prefix)

		mac := hmac.New(sha256.New, []byte(a.Secret()))
		mac.Write(body)
		sig := mac.Sum(nil)

		var expected []byte
		var err error
		if a.Encoding == "base64" {
			expected, err = base64.RawURLEncoding.DecodeString(got)
			if err != nil {
				expected, err = base64.StdEncoding.DecodeString(got)
			}
		} else {
			expected, err = hex.DecodeString(got)
		}
		if err != nil {
			return fmt.Errorf("%w: could not decode signature: %v", config.ErrAuth, err)
		}
		if !hmac.Equal(sig, expected) {
			return config.ErrAuth
		}
		return nil
	default:
		return config.ErrAuth
	}
}

// trimPrefix strips case-insensitively; providers love "sha256=" vs "sha1=".
func trimPrefix(s, prefix string) string {
	if prefix == "" || len(s) < len(prefix) {
		return s
	}
	if strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):]
	}
	return s
}
