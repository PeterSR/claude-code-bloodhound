package projectconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a config file at dir/.bloodhound/config.json.
func write(t *testing.T, dir, body string) string {
	t.Helper()
	d := filepath.Join(dir, Dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, File)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// tree builds home/project/sub and points $HOME at home, so the walk has a
// real stopping point instead of running to the filesystem root.
func tree(t *testing.T) (home, project, sub string) {
	t.Helper()
	home = t.TempDir()
	project = filepath.Join(home, "dev", "myapp")
	sub = filepath.Join(project, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home, project, sub
}

func TestLoadReturnsDefaultsWhenThereIsNoFile(t *testing.T) {
	// The common case by a wide margin, and the one that must not be an
	// error: almost every directory on the machine has never heard of this.
	_, _, sub := tree(t)

	cfg, found, err := Load(sub)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found.Path != "" {
		t.Errorf("Found.Path = %q, want empty", found.Path)
	}
	if cfg != Default() {
		t.Errorf("cfg = %+v, want the defaults %+v", cfg, Default())
	}
	if cfg.WriteupNudge.Enabled || cfg.CacheNudge.Enabled || cfg.Wakeup.Mode != WakeupOff {
		t.Error("something defaults to on; everything that speaks must be opt-in")
	}
}

func TestLoadWalksUpToTheProject(t *testing.T) {
	// A session started three directories down is still working in the
	// project, and the project's file is the one that governs it.
	_, project, sub := tree(t)
	want := write(t, project, `{"writeup_nudge": {"enabled": true}}`)

	cfg, found, err := Load(sub)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found.Path != want {
		t.Errorf("Found.Path = %q, want %q", found.Path, want)
	}
	if !cfg.WriteupNudge.Enabled {
		t.Error("WriteupNudge.Enabled = false, want true")
	}
	if cfg.Wakeup.Mode != WakeupOff {
		t.Errorf("Wakeup.Mode = %q, want %q: an absent key keeps its default", cfg.Wakeup.Mode, WakeupOff)
	}
}

func TestLoadPrefersTheNearestFile(t *testing.T) {
	// A directory of related repos may carry one file for all of them, and a
	// single repo inside it may still want to differ.
	home, project, sub := tree(t)
	write(t, filepath.Join(home, "dev"), `{"wakeup": {"mode": "nudge"}}`)
	want := write(t, project, `{"writeup_nudge": {"enabled": true}}`)

	cfg, found, err := Load(sub)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found.Path != want {
		t.Errorf("Found.Path = %q, want the nearer %q", found.Path, want)
	}
	// Nearest wins outright. This is a search, not a merge: the file that is
	// found is the whole answer, and a key it leaves out takes the built-in
	// default rather than a value from further up.
	if cfg.Wakeup.Mode != WakeupOff {
		t.Errorf("Wakeup.Mode = %q: a key from the outer file leaked into the nearer one's answer", cfg.Wakeup.Mode)
	}
}

func TestFindStopsAtHome(t *testing.T) {
	// Above home are directories shared with the rest of the system. A file up
	// there would govern every project at once, which is the global config's
	// job and not this one's.
	home, _, sub := tree(t)
	if err := os.MkdirAll(filepath.Join(home, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Dir(home), `{"writeup_nudge": {"enabled": true}}`)

	got, err := Find(sub)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != "" {
		t.Errorf("Find = %q, want none: the walk went above home", got)
	}
}

func TestFindReadsHomeItself(t *testing.T) {
	// Home is checked before the walk stops, so someone can set a default for
	// everything they work on without it being a global-config change.
	home, _, sub := tree(t)
	want := write(t, home, `{"writeup_nudge": {"enabled": true}}`)

	got, err := Find(sub)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != want {
		t.Errorf("Find = %q, want %q", got, want)
	}
}

func TestLoadReportsUnknownKeys(t *testing.T) {
	// The schemas being disjoint is the whole design, which makes putting a
	// global key in here a mistake worth naming: it parses, it is ignored, and
	// nothing would otherwise say so.
	_, project, _ := tree(t)
	write(t, project, `{"writeup_nudge": {"enabled": true, "mesage": "typo"},
	                   "claude_binary": "/bin/false", "poll_interval_s": 60}`)

	cfg, found, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.WriteupNudge.Enabled {
		t.Error("the key this build does understand was dropped")
	}
	// Nested as well as top level. A misspelled key inside an object is the
	// likelier mistake of the two now that the schema has objects in it, and
	// it fails exactly as silently.
	want := []string{"claude_binary", "poll_interval_s", "writeup_nudge.mesage"}
	if len(found.UnknownKeys) != len(want) {
		t.Fatalf("UnknownKeys = %v, want %v", found.UnknownKeys, want)
	}
	for i := range want {
		if found.UnknownKeys[i] != want[i] {
			t.Errorf("UnknownKeys = %v, want %v", found.UnknownKeys, want)
			break
		}
	}
}

func TestLoadRefusesAFileItCannotParse(t *testing.T) {
	// Someone put that file there on purpose. Falling back to the defaults
	// without a word would be the same as not having read it.
	_, project, _ := tree(t)
	path := write(t, project, `{"writeup_nudge": {"enabled": tru`)

	cfg, found, err := Load(project)
	if err == nil {
		t.Fatal("a broken file parsed clean")
	}
	if found.Path != path {
		t.Errorf("Found.Path = %q, want %q: the error must still name the file", found.Path, path)
	}
	if cfg != Default() {
		t.Errorf("cfg = %+v, want the defaults on a parse failure", cfg)
	}
}

func TestABadTemplateNeverSilencesAWarning(t *testing.T) {
	// The failure that matters. A project decorating its budget warning with
	// its own wording must not be able to lose the warning by mistyping the
	// decoration, because the mistake shows up as a silence and a silence is
	// indistinguishable from nothing being wrong.
	_, project, _ := tree(t)
	write(t, project, `{"pressure": {"message": "{{.Text} and a stray brace"},
	                   "cache_nudge": {"enabled": true, "message": "{{.Text}} fine"}}`)

	cfg, found, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Pressure.Message != "" {
		t.Errorf("Pressure.Message = %q, want it neutralised", cfg.Pressure.Message)
	}
	if len(found.Problems) != 1 {
		t.Fatalf("Problems = %v, want exactly one", found.Problems)
	}
	if !strings.Contains(found.Problems[0], "pressure.message") {
		t.Errorf("Problems = %v, want the offending key named", found.Problems)
	}
	// Everything else in the file still stands. One bad template is not a
	// reason to throw the rest away.
	if !cfg.CacheNudge.Enabled || cfg.CacheNudge.Message == "" {
		t.Error("a valid key was dropped along with the broken one")
	}
}

func TestAnUnknownWakeupModeFallsBackToOff(t *testing.T) {
	// Silence is the safe direction for a mode nobody can act on: the modes
	// that are not "off" both put extra words into a conversation, and one of
	// them makes a promise bloodhound would then have to keep.
	_, project, _ := tree(t)
	write(t, project, `{"wakeup": {"mode": "resune"}}`)

	cfg, found, err := Load(project)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Wakeup.Mode != WakeupOff {
		t.Errorf("Wakeup.Mode = %q, want %q", cfg.Wakeup.Mode, WakeupOff)
	}
	if cfg.Wakeup.Arms() || cfg.Wakeup.Suggests() {
		t.Error("a misspelled mode still did something")
	}
	if len(found.Problems) != 1 || !strings.Contains(found.Problems[0], "wakeup.mode") {
		t.Errorf("Problems = %v, want the mode named", found.Problems)
	}
}

func TestRenderFallsBackRatherThanFailing(t *testing.T) {
	// Render is the last line of the same defence as the parse check above,
	// for the failures a parse cannot see: a field that is not there, a
	// template that renders to nothing at all.
	v := Vars{Text: "the built-in sentence", Bucket: "5h"}

	if got, err := Render("", v, v.Text); got != v.Text || err != nil {
		t.Errorf("empty template: got %q, %v", got, err)
	}
	if got, _ := Render("{{.Bucket}} window", v, v.Text); got != "5h window" {
		t.Errorf("got %q, want the rendered template", got)
	}
	if got, err := Render("{{.NotAField}}", v, v.Text); got != v.Text || err == nil {
		t.Errorf("missing field: got %q, %v; want the fallback and an error", got, err)
	}
	// A template that renders to whitespace is almost always a conditional
	// the author expected to fire. Saying nothing would make the message
	// vanish on exactly the branch they did not test.
	if got, _ := Render("{{if .Session}}only when set{{end}}", v, v.Text); got != v.Text {
		t.Errorf("empty render: got %q, want the fallback", got)
	}
}
