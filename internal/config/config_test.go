package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, yml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSMTPDefaults(t *testing.T) {
	cfg, err := LoadConfig(write(t, `
services:
  mail:
    from: {}
    to:
      kind: smtp
      host: relay.internal
      from: bifrost@internal
flows:
  f:
    from: {service: mail}
    to: {service: mail}
    map: '{"x": 1}'
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Services["mail"].To
	if s.TLSMode != "starttls" || s.Port != 587 {
		t.Errorf("expected starttls/587 defaults, got %s/%d", s.TLSMode, s.Port)
	}
}

func TestSMTPImplicitTLSPort(t *testing.T) {
	cfg, err := LoadConfig(write(t, `
services:
  mail:
    from: {}
    to:
      kind: smtp
      host: relay.internal
      from: bifrost@internal
      tls: tls
flows:
  f:
    from: {service: mail}
    to: {service: mail}
    map: '{"x": 1}'
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Services["mail"].To
	if s.Port != 465 {
		t.Errorf("expected implicit TLS default port 465, got %d", s.Port)
	}
}

func TestSMTPValidation(t *testing.T) {
	if _, err := LoadConfig(write(t, `
services:
  mail: {to: {kind: smtp, host: relay.internal}}
flows:
  f: {from: {service: mail}, to: {service: mail}, map: '{"x": 1}'}
`)); err == nil {
		t.Error("expected error for missing smtp from")
	}
	if _, err := LoadConfig(write(t, `
services:
  mail: {to: {kind: smtp, host: relay, from: a@x, tls: bogus}}
flows:
  f: {from: {service: mail}, to: {service: mail}, map: '{"x": 1}'}
`)); err == nil {
		t.Error("expected error for invalid smtp tls mode")
	}
}

func TestMQTTValidation(t *testing.T) {
	if _, err := LoadConfig(write(t, `
services:
  bus: {to: {kind: mqtt}}
flows:
  f: {from: {service: bus}, to: {service: bus}, map: '{"x": 1}'}
`)); err == nil {
		t.Error("expected error for missing broker")
	}
	if _, err := LoadConfig(write(t, `
services:
  bus: {to: {kind: mqtt, broker: tcp://m:1883, qos: 5}}
flows:
  f: {from: {service: bus}, to: {service: bus}, map: '{"x": 1}'}
`)); err == nil {
		t.Error("expected error for invalid qos")
	}
}

func TestMQTTClientIDDefault(t *testing.T) {
	cfg, err := LoadConfig(write(t, `
services:
  bus:
    from: {}
    to:
      kind: mqtt
      broker: tcp://m:1883
flows:
  f: {from: {service: bus}, to: {service: bus}, map: '{"x": 1}'}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Services["bus"].To
	if s.ClientID != "bifrost" || s.QoS != 1 {
		t.Errorf("expected client_id bifrost default and qos 1, got %q/%d", s.ClientID, s.QoS)
	}
}
