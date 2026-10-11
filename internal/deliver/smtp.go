package deliver

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/smtp"
	"strings"
	"time"

	"bifrost/internal/config"
)

// deliverSMTP sends the mapped payload as an email via the target's SMTP
// relay. Connection-level properties (host, from, TLS, auth) are config; the
// payload is the message:
//
//	to      string | string[]  recipients; falls back to config "to" (or
//	                           the flow endpoint), then errors
//	subject string             Subject: header
//	text    string             body as text/plain
//	html    string             body as text/html (wins over text)
//
// If neither text nor html is present, the whole mapped payload is
// pretty-printed as the text body, so a minimal map still yields a useful
// notification.
func (d *Dispatcher) deliverSMTP(o *config.ToService, endpoint string, mapped any) error {
	msg, recipients, err := buildMail(o, endpoint, mapped)
	if err != nil {
		return err
	}

	auth := smtp.Auth(nil)
	var user, pass string
	if o.Auth.Type == "basic" {
		user, pass = o.Auth.Basic()
		auth = smtp.PlainAuth("", user, pass, o.Host)
	}

	addr := fmt.Sprintf("%s:%d", o.Host, o.Port)

	switch o.TLSMode {
	case "tls":
		// Implicit TLS (typically 465): dial TLS first.
		conn, err := tls.Dial("tcp", addr, &tls.Config{
			ServerName:         o.Host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: o.InsecureTLS,
		})
		if err != nil {
			return fmt.Errorf("smtp connect: %w", err)
		}
		defer conn.Close()
		c, err := smtp.NewClient(conn, o.Host)
		if err != nil {
			return fmt.Errorf("smtp client: %w", err)
		}
		defer c.Close()
		return smtpSend(c, auth, o.From, recipients, msg)
	default:
		// none / starttls: smtp.SendMail upgrades to TLS automatically when
		// the server advertises STARTTLS; PLAIN auth requires TLS on the
		// wire, so a server without TLS needs kind auth none on port 25
		// relay style.
		return smtp.SendMail(addr, auth, o.From, recipients, msg)
	}
}

func smtpSend(c *smtp.Client, auth smtp.Auth, from string, to []string, msg []byte) error {
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL: %w", err)
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp RCPT %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	return c.Quit()
}

// buildMail composes the RFC 822 message and returns the recipient list.
func buildMail(o *config.ToService, endpoint string, mapped any) ([]byte, []string, error) {
	fields, ok := mapped.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("smtp target requires a JSON object payload")
	}

	recipients, err := mailRecipients(fields["to"], o.To, endpoint)
	if err != nil {
		return nil, nil, err
	}

	subject := sanitizeHeader(str(fields["subject"], "Bifrost event"))
	text := str(fields["text"], "")
	html := str(fields["html"], "")

	contentType := "text/plain; charset=utf-8"
	if html != "" {
		text, contentType = html, "text/html; charset=utf-8"
	} else if text == "" {
		pretty, _ := json.MarshalIndent(mapped, "", "  ")
		text = string(pretty)
	}

	var s strings.Builder
	now := time.Now()

	fmt.Fprintf(&s, "From: %s\r\n", sanitizeHeader(o.From))
	fmt.Fprintf(&s, "To: %s\r\n", strings.Join(recipients, ", ")) // recipients already sanitized
	fmt.Fprintf(&s, "Subject: %s\r\n", subject)
	fmt.Fprintf(&s, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&s, "Message-ID: <%d.bifrost@%s>\r\n",
		now.UnixNano(), sanitizeHeader(o.Host))
	fmt.Fprintf(&s, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&s, "Content-Type: %s\r\n", contentType)
	s.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	s.WriteString("\r\n")
	// Body: strip bare CRs and normalize to CRLF; SMTP bodies may not
	// contain a bare <LF> or <CR> outside a CRLF pair (RFC 5321 §2.3.8).
	s.WriteString(crlf(text))
	// Messages end with a terminating CRLF.
	if !strings.HasSuffix(s.String(), "\r\n") {
		s.WriteString("\r\n")
	}

	return []byte(s.String()), recipients, nil
}

// mailRecipients collects recipients from the payload (string or array of
// strings), the service fallback, or the flow endpoint.
func mailRecipients(v any, fallback, endpoint string) ([]string, error) {
	var out []string
	add := func(r string) {
		if r = sanitizeHeader(strings.TrimSpace(r)); r != "" {
			out = append(out, r)
		}
	}
	switch t := v.(type) {
	case nil:
	case string:
		add(t)
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				add(s)
			}
		}
	default:
		return nil, fmt.Errorf("payload 'to' must be a string or an array of strings, got %T", v)
	}
	if len(out) == 0 {
		add(fallback)
	}
	if len(out) == 0 {
		add(endpoint)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("smtp target needs a recipient: payload 'to', service 'to' or flow endpoint")
	}
	return out, nil
}

// sanitizeHeader strips newlines from user-provided header data to prevent
// SMTP header injection.
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// str returns the string from a payload field or a default.
func str(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

// crlf normalizes line endings to SMTP-required CRLFs.
func crlf(s string) string {
	if !strings.Contains(s, "\r") {
		// Fast path: only LFs present.
		if strings.Contains(s, "\n") {
			return strings.ReplaceAll(s, "\n", "\r\n")
		}
		return s + "\r\n"
	}
	s = strings.ReplaceAll(s, "\r\n", "\n") // collapse existing CRLF to LF
	s = strings.ReplaceAll(s, "\r", "\n")   // and stray CRs
	return strings.ReplaceAll(s, "\n", "\r\n")
}
