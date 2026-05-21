package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ErrExtractorVersionMismatch is returned by ParseExtractor when the file's
// version doesn't equal ExtractorVersion. Callers (specifically
// LoadExtractor) treat this distinctly from other validation errors so we
// can fall back to the bundled default rather than fail loudly.
var ErrExtractorVersionMismatch = errors.New("extractor version mismatch")

// ExtractorVersion is the on-disk schema version. Bump when the DSL grows
// new primitives or when a new field becomes load-bearing; older extractors
// are refused on load (LoadExtractor falls back to the bundled default and
// emits a self-heal hint).
//
// Bump history:
//
//	v1 — initial: session_pct / week_pct + optional reset strings
//	v2 — added session_reset_tz / week_reset_tz so reset times are parsed
//	     in the user's actual timezone instead of being treated as UTC
const ExtractorVersion = 2

// FieldRule is one named extraction in the DSL. Strictly regex-based so
// the language stays non-Turing-complete and trivially safe to evaluate.
//
// All regexes compile under Go's RE2 (no backreferences, no lookarounds,
// linear-time). Use case-insensitive `(?i)` and DOTALL `(?s)` flags as
// needed for the TUI's cursor-positioned text.
type FieldRule struct {
	Name     string `json:"name"`
	Type     string `json:"type"` // "int" | "string"
	Regex    string `json:"regex"`
	Group    int    `json:"group"` // 0 = whole match, 1 = first capture, ...
	Required bool   `json:"required"`
}

// Extractor is a complete ruleset for parsing a /usage panel dump.
type Extractor struct {
	Version     int    `json:"version"`
	GeneratedAt string `json:"generated_at,omitempty"`
	GeneratedBy string `json:"generated_by,omitempty"` // "default" | "claude"

	Fields []FieldRule `json:"fields"`

	compiled []compiledRule
}

type compiledRule struct {
	rule FieldRule
	re   *regexp.Regexp
}

// Validate checks the version, types, and that every regex compiles. Must
// be called before Apply (LoadExtractor / ParseExtractor invoke this).
func (e *Extractor) Validate() error {
	if e.Version != ExtractorVersion {
		return fmt.Errorf("%w: file is v%d, code expects v%d",
			ErrExtractorVersionMismatch, e.Version, ExtractorVersion)
	}
	if len(e.Fields) == 0 {
		return fmt.Errorf("extractor has no fields")
	}
	e.compiled = e.compiled[:0]
	seen := map[string]bool{}
	for i, f := range e.Fields {
		if f.Name == "" {
			return fmt.Errorf("field %d: empty name", i)
		}
		if seen[f.Name] {
			return fmt.Errorf("field %q: duplicate name", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case "int", "string":
		default:
			return fmt.Errorf("field %q: unknown type %q (want int|string)", f.Name, f.Type)
		}
		if f.Group < 0 {
			return fmt.Errorf("field %q: negative group", f.Name)
		}
		re, err := regexp.Compile(f.Regex)
		if err != nil {
			return fmt.Errorf("field %q: regex compile: %w", f.Name, err)
		}
		if f.Group > re.NumSubexp() {
			return fmt.Errorf("field %q: group %d exceeds NumSubexp %d", f.Name, f.Group, re.NumSubexp())
		}
		e.compiled = append(e.compiled, compiledRule{rule: f, re: re})
	}
	return nil
}

// Extracted is the result of applying an Extractor to a panel.
type Extracted struct {
	// Values keyed by field name. Type-converted per the rule's `type`.
	Values map[string]any `json:"values"`
	// Missing lists the names of *required* fields that didn't match
	// (or whose match couldn't be type-converted).
	Missing []string `json:"missing,omitempty"`
}

// Apply runs every rule against text and returns the typed values plus the
// list of unmet required fields. Never errors — failures land in Missing.
func (e *Extractor) Apply(text string) Extracted {
	out := Extracted{Values: map[string]any{}}
	for _, c := range e.compiled {
		m := c.re.FindStringSubmatch(text)
		if m == nil || c.rule.Group >= len(m) {
			if c.rule.Required {
				out.Missing = append(out.Missing, c.rule.Name)
			}
			continue
		}
		raw := strings.TrimSpace(m[c.rule.Group])
		switch c.rule.Type {
		case "int":
			v, err := strconv.Atoi(raw)
			if err != nil {
				if c.rule.Required {
					out.Missing = append(out.Missing, c.rule.Name)
				}
				continue
			}
			out.Values[c.rule.Name] = v
		case "string":
			if raw == "" && c.rule.Required {
				out.Missing = append(out.Missing, c.rule.Name)
				continue
			}
			out.Values[c.rule.Name] = raw
		}
	}
	return out
}

// ParseExtractor unmarshals JSON and validates in one shot.
func ParseExtractor(data []byte) (*Extractor, error) {
	var e Extractor
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("decode extractor: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}
