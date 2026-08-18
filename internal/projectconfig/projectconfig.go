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
//     directory. Whether to speak up before a compaction, whether a warning
//     may suggest arming a wakeup. These keys exist only here; there is no
//     global default to override because there is no global version of them.
//
// That disjointness is also what makes reading a file out of a working
// directory safe. A checked-in .bloodhound/config.json can make bloodhound
// quieter or chattier in that project and can do nothing else. It cannot
// point the daemon at another database or another binary, because those words
// mean nothing in this schema.
//
// Absent is the common case and never an error. Every key has a built-in
// default, and a project that has never heard of this file gets them.
package projectconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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
// Every field is opt-in and defaults to off. Both of these put words into
// someone's conversation, and a tool that starts talking because it was
// installed, rather than because it was asked to, is one people uninstall.
type Config struct {
	// WriteupNudge asks bloodhound to say something while there is still
	// context left to say it in: a reminder to write the session up before a
	// compaction takes the detail away. Off by default.
	WriteupNudge bool `json:"writeup_nudge"`

	// WakeupNudge lets a budget or limit warning add that the window reopens
	// at a known time and that a wakeup could be armed for it. Bloodhound
	// suggests; it never arms anything itself. Off by default.
	WakeupNudge bool `json:"wakeup_nudge"`
}

// Default is what a directory with no file of its own gets.
func Default() Config { return Config{} }

// Found is where a config came from, and what was wrong with it.
type Found struct {
	// Path is the file that was read, empty when none was found. It is not
	// always inside the directory that was asked about: the search walks up.
	Path string

	// UnknownKeys are keys in the file this build does not understand,
	// sorted. Reported rather than ignored, because the whole point of a
	// disjoint schema is that a global key put in here silently does nothing.
	UnknownKeys []string
}

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
// on purpose and silently ignoring it would be worse than saying so.
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

// unknownKeys reports keys present in the file that this build has no field
// for. Best effort: a file that did not parse never gets here.
func unknownKeys(data []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var out []string
	for k := range raw {
		if !knownKeys()[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// knownKeys reads the schema off the struct rather than repeating it, so a
// field added above cannot start reporting itself as unknown.
func knownKeys() map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}
