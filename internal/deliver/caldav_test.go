package deliver

import (
	"strings"
	"testing"
)

// TestCaldavBuild checks VTODO construction from mapped output.
func TestCaldavBuild(t *testing.T) {
	ics, path, err := buildVTODO(map[string]any{
		"uid":         "abc123",
		"summary":     `Fix, "login; \ bug` + "\n" + "second line",
		"description": strings.Repeat("long line ", 20),
		"due":         "2026-11-01",
		"priority":    float64(5),
	})
	if err != nil {
		t.Fatalf("buildVTODO: %v", err)
	}
	if path != "abc123.ics" {
		t.Errorf("expected abc123.ics, got %s", path)
	}
	for _, want := range []string{
		"UID:abc123\r\n",
		"DUE;VALUE=DATE:20261101\r\n",
		"PRIORITY:5\r\n",
		`SUMMARY:Fix\, "login\; \\ bug\nsecond line` + "\r\n",
	} {
		if !strings.Contains(ics, want) {
			t.Errorf("VTODO missing %q:\n%s", want, ics)
		}
	}
	if !allLinesFoldedUnderLimit(ics) {
		t.Errorf("some lines exceed 75 octets without folding:\n%s", ics)
	}
}

func TestCaldavDueDatetime(t *testing.T) {
	line, err := icalDue("2026-11-01T09:00:00+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if line != "DUE;VALUE=DATE-TIME:20261101T070000Z" {
		t.Errorf("unexpected DUE line: %s", line)
	}
}

func TestCaldavRequiresUID(t *testing.T) {
	if _, _, err := buildVTODO(map[string]any{"summary": "no uid"}); err == nil {
		t.Error("expected error for missing uid")
	}
	// Plane IDs may be numeric; strings only, today.
	if _, _, err := buildVTODO(map[string]any{"uid": float64(42)}); err == nil {
		t.Error("expected error for numeric uid")
	}
}

func allLinesFoldedUnderLimit(ics string) bool {
	const limit = 75
	for _, line := range strings.Split(ics, "\r\n") {
		if line != "" && len(line) > limit && !strings.HasPrefix(line, " ") {
			return false
		}
	}
	return true
}
