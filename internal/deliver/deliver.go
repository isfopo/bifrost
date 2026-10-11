// Package deliver sends transformed events to target services. Targets are
// pluggable adapters selected by "kind": log, http (generic), or caldav
// (VTODO objects).
package deliver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bifrost/internal/config"
	"bifrost/internal/flow"
)

// MaxBodySize caps how big a request/response body this service will read;
// anything bigger is almost certainly not a body it wants to process.
const MaxBodySize = 1 << 20 // 1 MiB

// Dispatcher sends a transformed event to a target service.
type Dispatcher struct {
	client *http.Client
}

// NewDispatcher creates a ready-to-use dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{client: &http.Client{}}
}

// Deliver dispatches the mapped payload. It is called without the request
// client context so in-flight deliveries survive the client hanging up.
func (d *Dispatcher) Deliver(oref config.ServiceRef, o *config.ToService, mapped any) error {
	body, err := json.Marshal(mapped)
	if err != nil {
		return fmt.Errorf("marshal outgoing payload: %w", err)
	}

	var endpoint string
	if oref.Endpoint != "" {
		endpoint, err = flow.ResolveEndpoint(oref.Endpoint, mapped)
		if err != nil {
			return err
		}
	}

	var err2 error
	switch o.Kind {
	case "log":
		return d.deliverLog(oref, endpoint, mapped)
	case "http":
		err2 = d.withRetries(o, func() error { return d.deliverHTTP(o, endpoint, body) })
	case "caldav":
		err2 = d.withRetries(o, func() error { return d.deliverCalDAV(o, endpoint, mapped) })
	case "smtp":
		err2 = d.withRetries(o, func() error { return d.deliverSMTP(o, endpoint, mapped) })
	case "mqtt":
		err2 = d.withRetries(o, func() error { return d.deliverMQTT(o, endpoint, mapped) })
	default:
		return fmt.Errorf("unknown target kind %q", o.Kind)
	}
	return err2
}

// withRetries retries with exponential backoff: only network errors and 5xx
// are retryable; 4xx responses are terminal (wrong config, bad auth, ...).
func (d *Dispatcher) withRetries(o *config.ToService, attempt func() error) error {
	const baseBackoff = 500 * time.Millisecond
	err := attempt()
	for a := 1; a < o.Attempts && err != nil; a++ {
		time.Sleep(baseBackoff << (a - 1))
		err = attempt()
	}
	return err
}

func (d *Dispatcher) deliverLog(oref config.ServiceRef, endpoint string, mapped any) error {
	pretty, _ := json.MarshalIndent(mapped, "", "  ")
	log.Printf("[log] flow target %s (endpoint=%q) would receive:\n%s",
		oref.Service, endpoint, pretty)
	return nil
}

func (d *Dispatcher) deliverHTTP(o *config.ToService, endpoint string, body []byte) error {
	u, method, err := outboundURL(o, endpoint, "POST")
	if err != nil {
		return err
	}

	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	applyToAuth(o.Auth, req)
	req.Header.Set("User-Agent", "Bifrost/0.1")

	client := &http.Client{Timeout: o.TimeoutDuration()}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBodySize))
		resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode >= 500 {
		log.Printf("[http] %s: server error %d, will retry", u, resp.StatusCode)
	}
	return fmt.Errorf("target responded %d", resp.StatusCode)
}

func (d *Dispatcher) deliverCalDAV(o *config.ToService, endpoint string, mapped any) error {
	ics, path, err := buildVTODO(mapped)
	if err != nil {
		return err
	}
	if endpoint != "" {
		// An explicit endpoint (possibly JSONata-produced) wins.
		path = endpoint
	}

	u := strings.TrimSuffix(o.BaseURL, "/") + "/" + strings.TrimPrefix(path, "/")
	rp, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("parse caldav url %q: %w", u, err)
	}
	if rp.Scheme != "https" && !isLoopbackHost(rp.Host) {
		return fmt.Errorf("caldav url must use https for non-loopback hosts: %q", u)
	}

	req, err := http.NewRequest(http.MethodPut, u, strings.NewReader(ics))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	// Ask the server to refuse overwriting an existing task unintentionally;
	// sending the same event twice (retry) hits 412, treated as success.
	req.Header.Set("If-None-Match", "*")
	for k, v := range o.Headers {
		req.Header.Set(k, v)
	}
	applyToAuth(o.Auth, req)
	req.Header.Set("User-Agent", "Bifrost/0.1")

	client := &http.Client{Timeout: o.TimeoutDuration()}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("caldav request failed: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, MaxBodySize))
		resp.Body.Close()
	}()

	// 2xx = stored. 412 Precondition Failed means the UID already exists — an
	// idempotent replay, not an error.
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusPreconditionFailed {
		return nil
	}
	if resp.StatusCode >= 500 {
		log.Printf("[caldav] %s: server error %d, will retry", u, resp.StatusCode)
	}
	return fmt.Errorf("caldav server responded %d", resp.StatusCode)
}

func applyToAuth(a config.ToAuth, req *http.Request) {
	switch a.Type {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+a.BearerToken())
	case "basic":
		user, pass := a.Basic()
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
}

// outboundURL joins base_url and any endpoint, or uses the full url field.
// Method lets caldav/http both fall back to a sane default.
func outboundURL(o *config.ToService, endpoint, defMethod string) (string, string, error) {
	method := strings.ToUpper(o.Method)
	if method == "" {
		method = defMethod
	}
	u := o.URL
	if o.BaseURL != "" {
		u = strings.TrimSuffix(o.BaseURL, "/")
		if endpoint != "" {
			u += "/" + strings.TrimPrefix(endpoint, "/")
		}
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return "", "", fmt.Errorf("parse target url %q: %w", u, err)
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Host) {
		return "", "", fmt.Errorf("target url must use https for non-loopback hosts: %q", u)
	}
	return u, method, nil
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if host == "::1" || host == "[::1]" {
		return true
	}
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		host = strings.TrimSuffix(hostport[:i], ":") // strip :port
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1"
}
