// Package config defines Bifrost's YAML configuration and loads it,
// resolving secrets from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level YAML configuration for Bifrost.
type Config struct {
	Addr     string              `yaml:"addr"`
	Services map[string]*Service `yaml:"services"`
	Flows    map[string]*Flow    `yaml:"flows"`
}

// Service holds the reusable halves of a service definition. A service may
// define a "from" side (how webhooks arriving from it are authenticated),
// a "to" side (how to deliver to it), or both.
type Service struct {
	From *FromService `yaml:"from"`
	To   *ToService   `yaml:"to"`
}

// FromService describes the inbound half of a service.
type FromService struct {
	Auth FromAuth `yaml:"auth"`
}

// FromAuth verifies incoming webhook requests.
//   - none:   no verification (not recommended for anything internet-facing)
//   - header: the request must carry Header whose value equals the secret
//   - hmac:   HMAC-SHA256 over the raw body, compared to the value in Header
type FromAuth struct {
	Type      string `yaml:"type"`       // none | header | hmac
	Header    string `yaml:"header"`     // signing / token header name
	SecretEnv string `yaml:"secret_env"` // env var holding the secret
	Prefix    string `yaml:"prefix"`     // optional value prefix, e.g. "sha256="
	Encoding  string `yaml:"encoding"`   // hex (default) | base64
	secret    string // resolved from SecretEnv at load
}

// Secret returns the resolved inbound auth secret.
func (a *FromAuth) Secret() string {
	return a.secret
}

// ToService describes the outbound half of a service.
type ToService struct {
	// kind selects the adapter:
	//   log    - write the mapped payload to the log (great while authoring)
	//   http   - generic HTTP request
	//   caldav - PUT a VTODO (iCalendar TASK) object
	Kind     string            `yaml:"kind"`
	BaseURL  string            `yaml:"base_url"` // required for http/caldav ("http" kind may use full "url")
	URL      string            `yaml:"url"`      // full URL, only for kind: http
	Method   string            `yaml:"method"`   // http kind, default POST
	Headers  map[string]string `yaml:"headers"`
	Auth     ToAuth            `yaml:"auth"`
	Timeout  string            `yaml:"timeout"`  // e.g. 10s (default: 10s)
	Attempts int               `yaml:"attempts"` // delivery attempts incl. first (default: 3)
	timeout  time.Duration
}

// TimeoutDuration returns the parsed outbound timeout.
func (t *ToService) TimeoutDuration() time.Duration {
	return t.timeout
}

// ToAuth authenticates Bifrost towards a target service.
type ToAuth struct {
	Type        string `yaml:"type"` // none (default) | bearer | basic
	TokenEnv    string `yaml:"token_env"`
	UsernameEnv string `yaml:"username_env"`
	PasswordEnv string `yaml:"password_env"`

	token, username, password string
}

// BearerToken returns the resolved bearer token.
func (a *ToAuth) BearerToken() string {
	return a.token
}

// Basic returns the resolved username and password.
func (a *ToAuth) Basic() (string, string) {
	return a.username, a.password
}

// ServiceRef is a flow's reference to one side of a named service.
type ServiceRef struct {
	Service  string `yaml:"service"`
	Endpoint string `yaml:"endpoint"` // optional path; if it contains "$" it is
	// evaluated as a JSONata expression against the mapped output.
}

// Flow is a single webhook wire: from (source service) to (target service).
type Flow struct {
	From ServiceRef `yaml:"from"`
	To   ServiceRef `yaml:"to"`
	When string     `yaml:"when"` // optional JSONata filter; absent = always route
	Map  string     `yaml:"map"`  // JSONata transform body -> outgoing payload
}

// LoadConfig reads and validates the YAML file at path. Secrets referenced by
// *_env fields are resolved from the environment here; a missing value is a
// startup error so misconfiguration is caught before accepting traffic.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate(path string) error {
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.Services == nil {
		c.Services = map[string]*Service{}
	}

	knownFrom := map[string]bool{"none": true, "header": true, "hmac": true}
	for name, svc := range c.Services {
		if svc.From != nil {
			a := &svc.From.Auth
			if a.Type == "" {
				a.Type = "none"
			}
			if !knownFrom[a.Type] {
				return fmt.Errorf("%s: service %q: unknown from auth type %q", path, name, a.Type)
			}
			if a.Type != "none" {
				if a.Header == "" || a.SecretEnv == "" {
					return fmt.Errorf("%s: service %q: from auth %q needs header and secret_env", path, name, a.Type)
				}
				s, err := envRequired(name, a.SecretEnv)
				if err != nil {
					return err
				}
				a.secret = s
				if a.Encoding == "" {
					a.Encoding = "hex"
				}
				if a.Encoding != "hex" && a.Encoding != "base64" {
					return fmt.Errorf("%s: service %q: from auth encoding must be hex or base64", path, name)
				}
			}
		}
		if svc.To != nil {
			t := svc.To
			switch t.Kind {
			case "log":
			case "http", "caldav":
				if t.Kind == "http" && t.URL == "" && t.BaseURL == "" {
					return fmt.Errorf("%s: service %q: http target needs url or base_url", path, name)
				}
				if t.Kind == "caldav" && t.BaseURL == "" {
					return fmt.Errorf("%s: service %q: caldav target needs base_url", path, name)
				}
			default:
				return fmt.Errorf("%s: service %q: unknown target kind %q", path, name, t.Kind)
			}
			if t.Timeout == "" {
				t.Timeout = "10s"
			}
			d, err := time.ParseDuration(t.Timeout)
			if err != nil {
				return fmt.Errorf("%s: service %q: bad timeout %q: %w", path, name, t.Timeout, err)
			}
			t.timeout = d
			if t.Attempts <= 0 {
				t.Attempts = 3
			}
			if err := validateToAuth(path, name, &t.Auth); err != nil {
				return err
			}
		}
	}

	if len(c.Flows) == 0 {
		return fmt.Errorf("%s: at least one flow is required", path)
	}
	for name, flow := range c.Flows {
		if flow.From.Service == "" || flow.To.Service == "" {
			return fmt.Errorf("%s: flow %q: from.service and to.service are required", path, name)
		}
		svc := c.Services[flow.From.Service]
		if svc == nil || svc.From == nil {
			return fmt.Errorf("%s: flow %q: service %q has no 'from' definition", path, name, flow.From.Service)
		}
		svc = c.Services[flow.To.Service]
		if svc == nil || svc.To == nil {
			return fmt.Errorf("%s: flow %q: service %q has no 'to' definition", path, name, flow.To.Service)
		}
	}
	return nil
}

func validateToAuth(path, name string, a *ToAuth) error {
	switch a.Type {
	case "", "none":
		return nil
	case "bearer":
		s, err := envRequired(name, a.TokenEnv)
		if err != nil {
			return err
		}
		a.token = s
	case "basic":
		u, err := envRequired(name, a.UsernameEnv)
		if err != nil {
			return err
		}
		p, err := envRequired(name, a.PasswordEnv)
		if err != nil {
			return err
		}
		a.username, a.password = u, p
	default:
		return fmt.Errorf("%s: service %q: unknown to auth type %q", path, name, a.Type)
	}
	return nil
}

func envRequired(service, key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("service %q: environment variable %s is not set", service, key)
	}
	return v, nil
}

// ErrAuth is returned when an incoming request fails verification.
var ErrAuth = errors.New("authentication failed")
