package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bifrost/internal/config"
)

// loadTestServer compiles a config from YAML text and serves it on a
// httptest.Server; an optional env var is set for secret resolution.
func loadTestServer(t *testing.T, yml, envKey, envVal string) *httptest.Server {
	t.Helper()
	if envKey != "" {
		t.Setenv(envKey, envVal)
	}
	path := writeConfig(t, yml)
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func writeConfig(t *testing.T, yml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const planeBody = `{
  "event": "issue.created",
  "workspace": {"name": "Camping"},
  "issue": {
    "id": "abc123",
    "title": "Fix login bug",
    "description": "It is broken",
    "target_date": "2026-11-01"
  }
}`

const planeConfig = `
services:
  hooklog:
    from: {}
    to: {kind: log}
flows:
  plane-test:
    from: {service: hooklog}
    to: {service: hooklog}
    when: $.event = "issue.created"
    map: |
      {
        "uid": $.issue.id,
        "summary": $.issue.title,
        "due": $.issue.target_date
      }
`

func postJSON(t *testing.T, ts *httptest.Server, path, payload, sigHeader, sig string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sigHeader != "" {
		req.Header.Set(sigHeader, sig)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestDryRun exercises the /test authoring endpoint end to end.
func TestDryRun(t *testing.T) {
	ts := loadTestServer(t, planeConfig, "", "")
	resp := postJSON(t, ts, "/hooks/plane-test/test", planeBody, "", "")
	defer resp.Body.Close()

	var out struct {
		Matched bool           `json:"matched"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Matched {
		t.Errorf("expected matched=true, got %v", out)
	}
}

// TestNoMatchFilter checks that 'when' filters routes without delivery.
func TestNoMatchFilter(t *testing.T) {
	ts := loadTestServer(t, planeConfig, "", "")
	resp := postJSON(t, ts, "/hooks/plane-test", `{"event":"issue.deleted","issue":{}}`, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("expected 202 for non-matching event, got %d", resp.StatusCode)
	}
}

// TestFullFlow verifies when/map results against real Plane-shaped data.
func TestFullFlow(t *testing.T) {
	ts := loadTestServer(t, planeConfig, "", "")
	resp := postJSON(t, ts, "/hooks/plane-test/test", planeBody, "", "")
	defer resp.Body.Close()

	var out struct {
		Matched bool           `json:"matched"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Payload["uid"] != "abc123" {
		t.Errorf("expected uid=abc123, got %v", out.Payload["uid"])
	}
	if out.Payload["summary"] != "Fix login bug" {
		t.Errorf("expected summary 'Fix login bug', got %v", out.Payload["summary"])
	}
	if out.Payload["due"] != "2026-11-01" {
		t.Errorf("expected due 2026-11-01, got %v", out.Payload["due"])
	}
}

// TestHMACAuth covers prefix+hex signature verification.
func TestHMACAuth(t *testing.T) {
	yml := `
services:
  plane:
    from:
      auth:
        type: hmac
        header: X-Plane-Signature
        secret_env: TEST_SECRET
        prefix: "sha256="
  hooklog:
    to: {kind: log}
flows:
  plane-test:
    from: {service: plane}
    to: {service: hooklog}
    map: |
      {"uid": $.issue.id}
`
	ts := loadTestServer(t, yml, "TEST_SECRET", "topsecret")

	resp := postJSON(t, ts, "/hooks/plane-test", planeBody,
		"X-Plane-Signature", "sha256="+signHex("wrong-secret", planeBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for bad signature, got %d", resp.StatusCode)
	}

	resp = postJSON(t, ts, "/hooks/plane-test", planeBody,
		"X-Plane-Signature", "sha256="+signHex("topsecret", planeBody))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 at live endpoint, got %d", resp.StatusCode)
	}
}

// TestUnknownFlow verifies an unknown route path is a 404.
func TestUnknownFlow(t *testing.T) {
	ts := loadTestServer(t, planeConfig, "", "")
	resp := postJSON(t, ts, "/hooks/nope", planeBody, "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for unknown flow, got %d", resp.StatusCode)
	}
}

func signHex(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}
