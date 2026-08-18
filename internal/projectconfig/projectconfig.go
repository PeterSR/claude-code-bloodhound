// Package projectconfig reads the .bloodhound directory a project keeps
// beside its code.
//
// This is not a layer over the global config and there is no precedence to
// work out between them. The two files answer different questions, and their
// key sets do not overlap by construction:
//
//   - The global config, internal/config, is how the daemon itself operates:
//     where the database lives, which claude binary to drive, how often to
//     poll. One process serves every project on the machine, and a directory
//     it happens to look at has no business redirecting any of that.
//
//   - This file is how bloodhound behaves toward sessions working in that
//     directory. Whether to speak up before a compaction, what words to use,
//     whether a warning may suggest arming a wakeup or whether bloodhound
//     should carry the wakeup itself. These keys exist only here; there is no
//     global default to override because there is no global version of them.
//
// That disjointness is also what makes reading a file out of a working
// directory safe. A checked-in .bloodhound/config.json can make bloodhound
// quieter, chattier, or differently worded in that project and can do nothing
// else. It cannot point the daemon at another database or another binary,
// because those words mean nothing in this schema, and the one thing it can
// schedule is a message to the session that asked for it.
//
// Absent is the common case and never an error. Every key has a built-in
// default, and a project that has never heard of this file gets them.
//
// # Shape
//
// One object per thing bloodhound can say, rather than a flat list of
// booleans. Each of the four is a separate decision with its own trigger, its
// own audience and its own wording, and nesting is what keeps "may it speak"
// next to "what does it say" instead of scattering _message keys across the
// top level as the set grows:
//
//	{
//	  "pressure":      { "message": "" },
//	  "writeup_nudge": { "enabled": false, "message": "" },
//	  "cache_nudge":   { "enabled": false, "message": "" },
//	  "wakeup":        { "mode": "off", "nudge_message": "", "armed_message": "",
//	                     "resume_message": "" }
//	}
//
// # Templates
//
// Every text field is a Go text/template rendered against Vars, so a project
// can put the live numbers into its own wording rather than choosing between
// bloodhound's sentence and a static string. A field left empty means "use the
// built-in wording", which is what almost every project wants.
//
// Every one of them replaces what bloodhound would have said, and every one of
// them is handed that sentence as {{.Text}}. There is deliberately no separate
// key for adding to a message rather than replacing it, because {{.Text}}
// already expresses it and better: "{{.Text}} Push the branch first." says
// where the addition goes, which a bare append key has to decide on the
// project's behalf and will sometimes decide wrong.
//
// A template that does not parse, or that fails while rendering, never
// silences anything. It is reported as a problem by Load and by `bloodhound
// project show`, and the built-in wording is used instead. The alternative,
// dropping the message, would mean a typo in a nudge quietly disables the
// warning that the nudge was decorating.
package projectconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"
	"time"
)

// Dir is the directory a project keeps its bloodhound settings in, and File
// is the settings file inside it. A directory rather than a bare dotfile
// because it is the obvious home for anything else a project turns out to
// want to keep beside its code.
const (
	Dir  = ".bloodhound"
	File = "config.json"
)


// Config is what a project may say about how bloodhound treats its sessions.
//
// Everything that puts words into a conversation is opt-in and defaults to
// off. A tool that starts talking because it was installed, rather than
// because it was asked to, is one people uninstall. The one exception is
// Pressure, which does not decide whether bloodhound speaks (a limit that
// stops every session on the machine is not something a directory switches
// off) but only how the sentence reads.
type Config struct {
	// Pressure customises the wording of the warnings a project does not get
	// to opt out of: its own budget going tight, a meter projected to hit the
	// cap, a meter saturating. Empty by default, which leaves bloodhound's own
	// wording intact.
	Pressure Pressure `json:"pressure"`

	// WriteupNudge asks bloodhound to say something while there is still
	// context left to say it in: a reminder to write the session up before a
	// compaction takes the detail away. Off by default.
	WriteupNudge Nudge `json:"writeup_nudge"`

	// CacheNudge is the one line bloodhound will say to a session that is not
	// working. It fires while the prompt cache is still warm but inside its
	// last stretch, which is the only moment where interrupting an idle
	// session is cheaper than leaving it alone: the turn it starts is paid at
	// warm-cache rates, and the alternative is that the same context is
	// rebuilt from scratch later. Off by default.
	CacheNudge Nudge `json:"cache_nudge"`

	// Wakeup is what happens at the far end of a warning: nothing, a
	// suggestion that the reader arm something, or bloodhound noting the
	// session down and writing to it itself when the window reopens.
	Wakeup Wakeup `json:"wakeup"`
}

// Pressure is the wording of a warning that is going out regardless.
//
// No Enabled field, unlike the two nudges below. A limit that stops every
// session on the machine is not something a directory switches off, so the
// only question here is what the sentence says.
type Pressure struct {
	// Message stands in for bloodhound's own sentences, which are handed back
	// as {{.Text}}. A project that wants to add its standing instruction
	// rather than reword anything writes "{{.Text}} Push the branch first.",
	// which is also how it says whether the instruction comes before or after
	// the facts.
	//
	// It covers the pressure lines only, not the wakeup line underneath or
	// the opt-in nudges after it. Those have their own wording, and folding
	// them all into one template would mean a project that wanted to reword
	// its budget warning had to reproduce the rest of the message to keep it.
	Message string `json:"message"`
}

// Nudge is one optional line: whether to say it, and what to say.
type Nudge struct {
	// Enabled turns the line on. Off means bloodhound never says it here.
	Enabled bool `json:"enabled"`

	// Message replaces the built-in wording. The built-in line is available
	// as {{.Text}}. Empty means use the built-in line.
	Message string `json:"message"`
}

// Wakeup modes.
const (
	// WakeupOff says nothing about the far side of the window.
	WakeupOff = "off"
	// WakeupNudge adds a line saying the work could pick up again when the
	// window reopens, if something is armed to wake it. Bloodhound suggests
	// and arms nothing, because it has no idea what the reader schedules
	// wakeups with.
	WakeupNudge = "nudge"
	// WakeupResume drops the suggestion and takes the job. The session is
	// noted down against the window that is constraining it, and when that
	// window reopens bloodhound writes to it again.
	WakeupResume = "resume"
)

// Wakeup is the far end of a warning.
//
// A mode rather than two booleans, because "suggest that you arm something"
// and "I will do it myself" are alternatives and a config that lets both be
// true has to answer what that means. It also leaves room for a mode that
// does something else later without another key appearing at the top level.
type Wakeup struct {
	// Mode is WakeupOff, WakeupNudge or WakeupResume. Empty reads as off.
	Mode string `json:"mode"`

	// NudgeMessage replaces the built-in suggestion under WakeupNudge.
	NudgeMessage string `json:"nudge_message"`

	// ArmedMessage replaces what WakeupResume says at the moment it notes the
	// session down, which is a promise rather than a suggestion: bloodhound
	// will write again when the window reopens.
	ArmedMessage string `json:"armed_message"`

	// ResumeMessage replaces what arrives when the window has reopened. This
	// is the only message bloodhound sends that is not attached to a
	// transition someone is awake for, so it is worth wording it as something
	// a session can act on cold.
	ResumeMessage string `json:"resume_message"`
}

// Arms reports whether this project wants bloodhound to carry the wakeup.
func (w Wakeup) Arms() bool { return w.Mode == WakeupResume }

// Suggests reports whether a warning may carry the suggestion to arm one.
func (w Wakeup) Suggests() bool { return w.Mode == WakeupNudge }

// Default is what a directory with no file of its own gets.
//
// Mode is spelled out rather than left empty so that the stub `bloodhound
// project init` writes shows one of the legal values, which is the closest
// thing JSON gives us to documenting an enum in place.
func Default() Config { return Config{Wakeup: Wakeup{Mode: WakeupOff}} }

// Found is where a config came from, and what was wrong with it.
type Found struct {
	// Path is the file that was read, empty when none was found. It is not
	// always inside the directory that was asked about: the search walks up.
	Path string

	// UnknownKeys are keys in the file this build does not understand, dotted
	// and sorted. Reported rather than ignored, because the whole point of a
	// disjoint schema is that a global key put in here silently does nothing.
	UnknownKeys []string

	// Problems are values this build understands the name of and cannot use:
	// a mode that is not a mode, a template that does not parse. The
	// offending field is neutralised in the returned Config and everything
	// else in the file still applies, so a typo in one line of wording cannot
	// take a warning down with it.
	Problems []string
}

// OK reports whether the file was read with nothing to complain about.
func (f Found) OK() bool { return len(f.UnknownKeys) == 0 && len(f.Problems) == 0 }

// Load resolves the config that governs work in dir.
//
// The search walks up from dir to the home directory, taking the first
// .bloodhound/config.json it finds. Walking up rather than checking only dir
// is what lets a session started in a subdirectory of a project still be
// governed by that project, and what lets a directory of related repos share
// one file.
//
// A missing file returns Default with an empty Found and no error. An
// unreadable or unparsable one is an error, because that file was put there
// on purpose and silently ignoring it would be worse than saying so. A file
// that parses but contains a value this build cannot use is not an error: the
// value is dropped, the rest of the file stands, and Found.Problems says what
// happened.
func Load(dir string) (Config, Found, error) {
	cfg := Default()

	path, err := Find(dir)
	if err != nil || path == "" {
		return cfg, Found{}, err
	}
	found := Found{Path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, found, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), found, fmt.Errorf("parse %s: %w", path, err)
	}
	found.UnknownKeys = unknownKeys(data)
	found.Problems = cfg.sanitise()
	return cfg, found, nil
}

// Find returns the path of the config governing dir, or "" when there is
// none. Exposed separately so `bloodhound doctor` can report which file is in
// play without caring what is in it.
func Find(dir string) (string, error) {
	start, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir() // best effort: an unknown home just means the walk runs to the root

	for cur := start; ; {
		candidate := filepath.Join(cur, Dir, File)
		switch _, err := os.Stat(candidate); {
		case err == nil:
			return candidate, nil
		case !errors.Is(err, fs.ErrNotExist):
			return "", fmt.Errorf("stat %s: %w", candidate, err)
		}
		// Home is checked and then stops the walk. Above it are directories
		// shared with the rest of the system, and a file up there would govern
		// every project at once, which is the global config's job.
		if home != "" && cur == home {
			return "", nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", nil
		}
		cur = parent
	}
}

// sanitise drops values this build cannot act on and says what it dropped.
//
// Neutralising in place rather than returning an error is the whole point: a
// project that misspells one template still gets its budget warnings, in
// bloodhound's own words, and finds out what is wrong from `project show`
// rather than from a silence it has no reason to investigate.
func (c *Config) sanitise() []string {
	var problems []string

	switch c.Wakeup.Mode {
	case "":
		c.Wakeup.Mode = WakeupOff
	case WakeupOff, WakeupNudge, WakeupResume:
	default:
		problems = append(problems, fmt.Sprintf(
			"wakeup.mode: %q is not one of %q, %q, %q (treated as %q)",
			c.Wakeup.Mode, WakeupOff, WakeupNudge, WakeupResume, WakeupOff))
		c.Wakeup.Mode = WakeupOff
	}

	for _, f := range c.templates() {
		if *f.text == "" {
			continue
		}
		if _, err := compile(*f.text); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v (built-in wording used instead)", f.key, err))
			*f.text = ""
		}
	}
	return problems
}

// templates is every text field with the dotted key it is written under, so
// validation and any future rendering pass do not have to repeat the list.
func (c *Config) templates() []struct {
	key  string
	text *string
} {
	return []struct {
		key  string
		text *string
	}{
		{"pressure.message", &c.Pressure.Message},
		{"writeup_nudge.message", &c.WriteupNudge.Message},
		{"cache_nudge.message", &c.CacheNudge.Message},
		{"wakeup.nudge_message", &c.Wakeup.NudgeMessage},
		{"wakeup.armed_message", &c.Wakeup.ArmedMessage},
		{"wakeup.resume_message", &c.Wakeup.ResumeMessage},
	}
}

// Vars is what a project's templates can talk about.
//
// One struct for every field rather than one per field, because the fields
// overlap heavily and a project moving a sentence from one key to another
// should not have to learn a second vocabulary. Anything that does not apply
// to the message being rendered is left at its zero value, so a template that
// reaches for it gets an empty string rather than an error.
type Vars struct {
	// Text is what bloodhound would have said here without the template. Any
	// field that stands in for built-in wording still offers it, so reframing
	// a sentence never means reproducing the numbers by hand.
	Text string

	// Kind and State are the transition behind the message: "budget" and
	// "tight", "limit_projection" and "projected", "saturation" and
	// "saturated", "recommendation" and "compact", "cache" and "expiring".
	// On a message covering several transitions they describe the newest one.
	Kind  string
	State string

	// Bucket is the window under pressure, worded for prose: "5h" or "week".
	// Empty on a message that is not about a window.
	Bucket string

	// Cwd is the session's working directory and Dir is its last element,
	// which is what the built-in wording uses. Project is bloodhound's name
	// for the project, which is not always the directory name.
	Cwd     string
	Dir     string
	Project string

	// Session is the session UUID the message is going to.
	Session string

	// Pct is the meter reading behind the message, 0 when there was none.
	Pct int

	// Reset is when the window reopens, worded the way the built-in lines
	// word it: "in 1h30m" under a day, "on fri 18:00" past one. ResetAt is
	// the same moment as a time, for a template that wants its own format.
	Reset   string
	ResetAt time.Time

	// ETA is when the meter is projected to reach the cap, worded the same
	// way. Empty unless the message is about a projection.
	ETA string

	// ArmedAt and Waited are set only on a resume message: when the session
	// was noted down, and how long ago that was.
	ArmedAt time.Time
	Waited  string

	// Now is the moment the message was built.
	Now time.Time
}

// Render runs one template against v, falling back to fallback.
//
// Every failure returns the fallback text and an error, and every caller in
// bloodhound sends the text and logs the error. The message going out in the
// wrong words is a nuisance; the message not going out is the failure the
// whole system exists to avoid.
func Render(tmpl string, v Vars, fallback string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		return fallback, nil
	}
	t, err := compile(tmpl)
	if err != nil {
		return fallback, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return fallback, err
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		// A template that renders to nothing is almost always a conditional
		// the author expected to fire. Treating it as "say nothing" would
		// make the message vanish on exactly the branch they did not test, so
		// it reads as "no opinion" and the built-in wording stands.
		return fallback, nil
	}
	return out, nil
}

// compile parses a template with the options that make a mistake loud.
//
// Option "missingkey=error" does not apply to struct fields, which are a
// compile-time error already; it is set for the map cases a future Vars field
// might introduce. Funcs are deliberately not extended: a project config is
// checked into a repo and read by a daemon, and the less it can express the
// less anyone has to think about what a pull request to it does.
func compile(tmpl string) (*template.Template, error) {
	return template.New("projectconfig").Option("missingkey=error").Parse(tmpl)
}

// unknownKeys reports keys present in the file that this build has no field
// for, dotted for the nested objects. Best effort: a file that did not parse
// never gets here.
func unknownKeys(data []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var out []string
	collectUnknown(reflect.TypeOf(Config{}), raw, "", &out)
	sort.Strings(out)
	return out
}

// collectUnknown walks the file and the schema together. It descends into a
// nested object only when the schema has a struct there, so a project that
// wrote a string where an object belongs is reported by the parse error
// rather than by a spurious list of unknown children.
func collectUnknown(t reflect.Type, raw map[string]json.RawMessage, prefix string, out *[]string) {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = f.Type
	}
	for k, v := range raw {
		ft, ok := fields[k]
		if !ok {
			*out = append(*out, prefix+k)
			continue
		}
		if ft.Kind() != reflect.Struct {
			continue
		}
		var child map[string]json.RawMessage
		if err := json.Unmarshal(v, &child); err != nil {
			continue
		}
		collectUnknown(ft, child, prefix+k+".", out)
	}
}
