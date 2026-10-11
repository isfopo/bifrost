package deliver

import (
	"fmt"
	"strings"
	"time"
)

// buildVTODO turns a mapped payload into a VTODO calendar object for a
// CalDAV PUT. The mapping yields the fields; the adapter owns the format.
//
// Expected mapped fields (all optional except uid):
//
//	uid         string  %s UID and default object path (<uid>.ics)
//	summary     string  SUMMARY (task title)
//	description string  DESCRIPTION
//	due         string  DUE; date "2026-11-01" -> VALUE=DATE, RFC3339 -> UTC
//	status      string  NEEDS-ACTION | IN-PROCESS | COMPLETED ...
//	priority    number  1-9 (lower = more urgent)
//	percent     number  0-100 percent complete
func buildVTODO(mapped any) (string, string, error) {
	fields, ok := mapped.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("caldav target requires a JSON object with at least a 'uid' field")
	}

	uid, ok := fields["uid"].(string)
	if !ok || uid == "" {
		return "", "", fmt.Errorf("caldav target requires a non-empty string 'uid' in the mapped payload")
	}
	uid = sanitizeICal(uid)

	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\n")
	b.WriteString("VERSION:2.0\r\n")
	b.WriteString("PRODID:-//Bifrost//EN\r\n")
	b.WriteString("BEGIN:VTODO\r\n")
	fmt.Fprintf(&b, "UID:%s\r\n", uid)
	fmt.Fprintf(&b, "DTSTAMP:%s\r\n", time.Now().UTC().Format(icalDateTime))

	if v, ok := fields["summary"].(string); ok && v != "" {
		writeICalLine(&b, "SUMMARY", v)
	}
	if v, ok := fields["description"].(string); ok && v != "" {
		writeICalLine(&b, "DESCRIPTION", v)
	}
	if v, ok := fields["due"].(string); ok && v != "" {
		if line, err := icalDue(v); err != nil {
			return "", "", fmt.Errorf("bad 'due' value %q: %w", v, err)
		} else {
			b.WriteString(line)
			b.WriteString("\r\n")
		}
	}
	if v, ok := fields["status"].(string); ok && v != "" {
		fmt.Fprintf(&b, "STATUS:%s\r\n", sanitizeICal(v))
	}
	if priority, err := intField(fields, "priority"); err != nil {
		return "", "", err
	} else if priority != 0 {
		fmt.Fprintf(&b, "PRIORITY:%d\r\n", priority)
	}
	if percent, err := intField(fields, "percent"); err != nil {
		return "", "", err
	} else if percent != 0 {
		fmt.Fprintf(&b, "PERCENT-COMPLETE:%d\r\n", percent)
	}

	b.WriteString("END:VTODO\r\n")
	b.WriteString("END:VCALENDAR\r\n")
	return b.String(), uid + ".ics", nil
}

func intField(fields map[string]any, key string) (int, error) {
	switch v := fields[key].(type) {
	case nil:
		return 0, nil
	case float64: // encoding/json numbers
		if v != float64(int(v)) {
			return 0, fmt.Errorf("field %q must be an integer, got %v", key, v)
		}
		return int(v), nil
	default:
		return 0, fmt.Errorf("field %q must be a number, got %T", key, v)
	}
}

const (
	icalDate     = "20060102"
	icalDateTime = "20060102T150405Z"
)

// icalDue builds the DUE line. Plain dates get VALUE=DATE; datetimes are
// converted to UTC floating form.
func icalDue(v string) (string, error) {
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return "DUE;VALUE=DATE:" + t.Format(icalDate), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		// Also allow local datetimes without a zone ("2026-11-01T09:00:00").
		if t, err2 := time.Parse("2006-01-02T15:04:05", v); err2 == nil {
			return "DUE;VALUE=DATE-TIME:" + t.UTC().Format(icalDateTime), nil
		}
		return "", fmt.Errorf("expected YYYY-MM-DD or RFC3339 datetime")
	}
	return "DUE;VALUE=DATE-TIME:" + t.UTC().Format(icalDateTime), nil
}

// sanitizeICal strips characters not allowed in property values; literal
// newlines in user data are the main concern (header/text injection into the
// calendar object).
func sanitizeICal(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}

// writeICalLine writes a TEXT property with RFC 5545 escaping and line folding
// (long lines are folded at 75 bytes with a leading space).
func writeICalLine(b *strings.Builder, name, value string) {
	var esc strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			esc.WriteString(`\\`)
		case ';':
			esc.WriteString(`\;`)
		case ',':
			esc.WriteString(`\,`)
		case '\r':
		case '\n':
			esc.WriteString(`\n`)
		default:
			esc.WriteRune(r)
		}
	}
	// The concatenation here is not avoidable waste: fold() needs the total
	// line (name + value) to measure and wrap it at 75 octets.
	fold(b, name+":"+esc.String())
}

// fold writes content lines split at 75 octets (RFC 5545 §3.1).
func fold(b *strings.Builder, line string) {
	const limit = 75
	for len(line) > limit {
		cut := limit
		for cut > 0 && line[cut] >= 0x80 && line[cut] < 0xC0 {
			cut-- // never split a UTF-8 sequence
		}
		if cut == 0 {
			cut = limit // avoid a loop on pathological input
		}
		b.WriteString(line[:cut])
		b.WriteString("\r\n ")
		line = line[cut:]
	}
	b.WriteString(line)
	b.WriteString("\r\n")
}
