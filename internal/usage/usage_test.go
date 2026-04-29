package usage

import (
	"strings"
	"testing"
	"time"
)

// fakePanel mirrors the real TUI's run-together-text + zero-whitespace-after-Resets
// shape. Real samples never appear in tracked tests.
const fakePanel = `
Currentsession
████████                                          16%used
Resets2am(Europe/Copenhagen)

Currentweek(allmodels)
██████████████████████████████▌                   61%used
ResetsMay1,1am(Europe/Copenhagen)

Currentweek(Sonnetonly)
                                                    0%used
ResetsMay1,1am(Europe/Copenhagen)
`

func loadDefault(t *testing.T) *Extractor {
	t.Helper()
	ex, err := ParseExtractor(defaultExtractorJSON)
	if err != nil {
		t.Fatalf("parse default extractor: %v", err)
	}
	return ex
}

func TestDefaultExtractor_RequiredFieldsExtract(t *testing.T) {
	ex := loadDefault(t)
	out := ex.Apply(fakePanel)
	if len(out.Missing) != 0 {
		t.Fatalf("required fields missing: %v", out.Missing)
	}
	if got := out.Values["session_pct"]; got != 16 {
		t.Errorf("session_pct: want 16, got %v (%T)", got, got)
	}
	if got := out.Values["week_pct"]; got != 61 {
		t.Errorf("week_pct: want 61, got %v (%T)", got, got)
	}
}

func TestDefaultExtractor_OptionalResets(t *testing.T) {
	ex := loadDefault(t)
	out := ex.Apply(fakePanel)
	if got, _ := out.Values["session_reset"].(string); got != "2am" {
		t.Errorf("session_reset: want %q, got %q", "2am", got)
	}
	if got, _ := out.Values["week_reset"].(string); got != "May1,1am" {
		t.Errorf("week_reset: want %q, got %q", "May1,1am", got)
	}
}

func TestDefaultExtractor_PicksWeekAllNotPerModel(t *testing.T) {
	ex := loadDefault(t)
	out := ex.Apply(fakePanel)
	// The third bucket (Sonnet-only) is at 0%; if our regex grabbed that
	// instead of the all-models 61% line, the test catches it.
	if got := out.Values["week_pct"]; got != 61 {
		t.Errorf("week_pct picked the wrong bucket; got %v", got)
	}
}

func TestExtractorValidate_RejectsUnknownVersion(t *testing.T) {
	ex := &Extractor{Version: 999, Fields: []FieldRule{{
		Name: "x", Type: "int", Regex: ".", Group: 0, Required: false,
	}}}
	if err := ex.Validate(); err == nil {
		t.Fatal("expected version error")
	}
}

func TestExtractorValidate_RejectsBadRegex(t *testing.T) {
	ex := &Extractor{Version: ExtractorVersion, Fields: []FieldRule{{
		Name: "x", Type: "int", Regex: "(", Group: 0,
	}}}
	if err := ex.Validate(); err == nil || !strings.Contains(err.Error(), "regex compile") {
		t.Fatalf("expected regex error, got %v", err)
	}
}

func TestExtractorValidate_RejectsGroupOutOfRange(t *testing.T) {
	ex := &Extractor{Version: ExtractorVersion, Fields: []FieldRule{{
		Name: "x", Type: "int", Regex: "abc", Group: 1,
	}}}
	if err := ex.Validate(); err == nil {
		t.Fatal("expected group error")
	}
}

func TestExtractorValidate_RejectsDuplicateNames(t *testing.T) {
	ex := &Extractor{Version: ExtractorVersion, Fields: []FieldRule{
		{Name: "x", Type: "int", Regex: "(\\d+)", Group: 1},
		{Name: "x", Type: "int", Regex: "(\\d+)", Group: 1},
	}}
	if err := ex.Validate(); err == nil {
		t.Fatal("expected duplicate-name error")
	}
}

func TestApply_RequiredMissing(t *testing.T) {
	ex := &Extractor{Version: ExtractorVersion, Fields: []FieldRule{
		{Name: "needed", Type: "int", Regex: `(\d+)\s*foo`, Group: 1, Required: true},
	}}
	if err := ex.Validate(); err != nil {
		t.Fatal(err)
	}
	out := ex.Apply("nothing here")
	if len(out.Missing) != 1 || out.Missing[0] != "needed" {
		t.Fatalf("Missing: want [needed], got %v", out.Missing)
	}
}

func TestStripANSI(t *testing.T) {
	in := []byte("\x1b[31mhello\x1b[0m\x1b7world\x1b8")
	got := stripANSI(in)
	if got != "helloworld" {
		t.Errorf("stripANSI: want %q, got %q", "helloworld", got)
	}
}

func TestParseReset_TimeOnly_NearestFuture(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	got, ok := ParseReset("2am", now)
	if !ok {
		t.Fatal("ParseReset(2am) returned ok=false")
	}
	want := time.Date(2026, 5, 2, 2, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(2am): want %s, got %s", want, got)
	}
}

func TestParseReset_DateAndTime(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	got, ok := ParseReset("May1,1am", now)
	if !ok {
		t.Fatal("ParseReset(May1,1am) returned ok=false")
	}
	want := time.Date(2026, 5, 1, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(May1,1am): want %s, got %s", want, got)
	}
}

func TestParseReset_PastBecomesNextOccurrence(t *testing.T) {
	now := time.Date(2026, 4, 28, 14, 0, 0, 0, time.UTC) // 2pm
	got, ok := ParseReset("1am", now)
	if !ok {
		t.Fatal("ParseReset(1am) returned ok=false")
	}
	want := time.Date(2026, 4, 29, 1, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("ParseReset(1am at 2pm): want %s, got %s", want, got)
	}
}

func TestParseReset_Empty(t *testing.T) {
	if _, ok := ParseReset("", time.Now()); ok {
		t.Error("ParseReset(\"\"): want ok=false")
	}
}

func TestParseReset_Garbage(t *testing.T) {
	if _, ok := ParseReset("not a time", time.Now()); ok {
		t.Error("ParseReset garbage: want ok=false")
	}
}
