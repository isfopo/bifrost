package deliver

import (
	"strings"
	"testing"

	"bifrost/internal/config"
)

func TestBuildMailPlainText(t *testing.T) {
	o := &config.ToService{
		Host: "smtp.internal",
		Port: 587,
		From: "bifrost@internal",
	}
	msg, rcpt, err := buildMail(o, "", map[string]any{
		"to":      "ops@internal",
		"subject": "New alert",
		"text":    "Something happened\nsecond line",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rcpt) != 1 || rcpt[0] != "ops@internal" {
		t.Errorf("recipients: %v", rcpt)
	}
	for _, want := range []string{
		"From: bifrost@internal\r\n",
		"To: ops@internal\r\n",
		"Subject: New alert\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n",
		"Something happened\r\nsecond line\r\n",
	} {
		if !strings.Contains(string(msg), want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

// TestBuildMailHeaderInjection verifies CRLF stripping in header values.
func TestBuildMailHeaderInjection(t *testing.T) {
	o := &config.ToService{Host: "h", From: "f@x", To: "safe@x"}
	msg, _, err := buildMail(o, "", map[string]any{
		"subject": "evil\nBcc: victim@x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Only headers must be clean; the (JSON) body reproducing the subject
	// text is harmless.
	headers := strings.SplitN(string(msg), "\r\n\r\n", 2)[0]
	for _, h := range strings.Split(headers, "\r\n") {
		if strings.HasPrefix(h, "Bcc:") || strings.HasPrefix(h, "bcc:") {
			t.Errorf("injected header survived:\n%s", h)
		}
	}
	if !strings.Contains(headers, "Subject: evilBcc: victim@x\r\n") {
		t.Errorf("sanitized subject not present:\n%s", headers)
	}
}

// TestBuildMailRecipients covers to-as-array, fallback, and endpoint.
func TestBuildMailRecipients(t *testing.T) {
	o := &config.ToService{Host: "h", From: "f@x", To: "fallback@x"}
	_, rcpt, err := buildMail(o, "", map[string]any{
		"to": []any{"a@x", "b@x"},
	})
	if err != nil || len(rcpt) != 2 {
		t.Fatalf("rcpt=%v err=%v", rcpt, err)
	}

	_, rcpt, err = buildMail(o, "", map[string]any{"subject": "s"})
	if err != nil || rcpt[0] != "fallback@x" {
		t.Fatalf("fallback recipient: %v %v", rcpt, err)
	}

	_, rcpt, err = buildMail(&config.ToService{Host: "h", From: "f@x"}, "endpoint@x",
		map[string]any{"subject": "s"})
	if err != nil || rcpt[0] != "endpoint@x" {
		t.Fatalf("endpoint recipient: %v %v", rcpt, err)
	}

	if _, _, err := buildMail(&config.ToService{Host: "h", From: "f@x"}, "", map[string]any{}); err == nil {
		t.Error("expected error with no recipient source")
	}
}

func TestBuildMailHTMLWins(t *testing.T) {
	o := &config.ToService{Host: "h", From: "f@x", To: "r@x"}
	msg, _, err := buildMail(o, "", map[string]any{
		"subject": "s", "text": "plain", "html": "<b>rich</b>",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), "Content-Type: text/html; charset=utf-8\r\n") {
		t.Errorf("html content type missing:\n%s", msg)
	}
	if !strings.Contains(string(msg), "<b>rich</b>") {
		t.Errorf("html body missing:\n%s", msg)
	}
}

func TestMQTTTopicResolution(t *testing.T) {
	got, err := mqttTopic("alerts/plane", "fallback")
	if err != nil || got != "alerts/plane" {
		t.Fatalf("endpoint topic: %q %v", got, err)
	}
	got, err = mqttTopic("", "fallback/bifrost")
	if err != nil || got != "fallback/bifrost" {
		t.Fatalf("fallback topic: %q %v", got, err)
	}
	if _, err = mqttTopic("", ""); err == nil {
		t.Error("expected error when no topic source")
	}
}
