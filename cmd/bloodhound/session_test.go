package main

import (
	"context"
	"testing"

	"github.com/PeterSR/claude-code-bloodhound/internal/store"
)

// TestCurrentSessionUUID_PresentAndAbsent covers both sides of the one
// signal this command trusts: the variable set (the normal case, running
// as a child of Claude Code) and unset (a plain shell, or any process
// Claude Code didn't spawn), with no fallback attempted in either case.
func TestCurrentSessionUUID_PresentAndAbsent(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		t.Setenv(claudeCodeSessionEnvVar, "4ba3954b-f57b-4d95-9320-bd91f2a45edd")
		uuid, ok := currentSessionUUID()
		if !ok {
			t.Fatalf("ok = false, want true when %s is set", claudeCodeSessionEnvVar)
		}
		if want := "4ba3954b-f57b-4d95-9320-bd91f2a45edd"; uuid != want {
			t.Errorf("uuid = %q, want %q", uuid, want)
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Setenv(claudeCodeSessionEnvVar, "")
		// t.Setenv can only set, not unset; "" is the same "not present"
		// state currentSessionUUID must treat as absent (os.Getenv returns
		// "" for both an unset variable and one explicitly set to empty,
		// and there is no real-world case where Claude Code would inject
		// an empty session id, so collapsing the two is correct here).
		uuid, ok := currentSessionUUID()
		if ok {
			t.Fatalf("ok = true, want false when %s is empty/unset (uuid=%q)", claudeCodeSessionEnvVar, uuid)
		}
		if uuid != "" {
			t.Errorf("uuid = %q, want \"\"", uuid)
		}
	})
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	s, err := store.Open(context.Background())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

// TestBuildSessionJSON_UnknownSession is the graceful-empty case: a uuid
// aggregate has never seen (still being written, or simply made up) must
// report Known=false rather than a blank Project/Cwd that could be
// mistaken for a real, empty value.
func TestBuildSessionJSON_UnknownSession(t *testing.T) {
	s := openTestStore(t)
	out, err := buildSessionJSON(context.Background(), s, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("buildSessionJSON: %v", err)
	}
	if out.Known {
		t.Errorf("Known = true, want false for a session aggregate has never materialized")
	}
	if out.Project != "" || out.Cwd != "" || out.Attribution != nil {
		t.Errorf("unknown session leaked data: %+v", out)
	}
}

// TestBuildSessionJSON_KnownSessionWithAttribution is the populated case:
// once aggregate has run, project/cwd come from the sessions table and
// attribution totals come from the same rollup "attribution session" uses,
// so the two commands can never disagree about one session's numbers.
func TestBuildSessionJSON_KnownSessionWithAttribution(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	const uuid = "11111111-1111-1111-1111-111111111111"

	if err := s.ReplaceSessions(ctx, []store.SessionRow{
		{SessionUUID: uuid, Project: "-home-user-projects-myapp", Cwd: "/home/user/projects/myapp"},
	}); err != nil {
		t.Fatalf("ReplaceSessions: %v", err)
	}
	if err := s.ReplaceAttribution(ctx, "week",
		[]store.LimitWindowRow{{Bucket: "week", StartUnixMS: 1000, EndUnixMS: 2000}},
		[]store.AttributionRow{{Bucket: "week", WindowStartUnixMS: 1000, SessionUUID: uuid, Project: "-home-user-projects-myapp", MeasuredPct: 12.5}},
	); err != nil {
		t.Fatalf("ReplaceAttribution: %v", err)
	}

	out, err := buildSessionJSON(ctx, s, uuid)
	if err != nil {
		t.Fatalf("buildSessionJSON: %v", err)
	}
	if !out.Known {
		t.Fatalf("Known = false, want true: %+v", out)
	}
	if got, want := out.Cwd, "/home/user/projects/myapp"; got != want {
		t.Errorf("Cwd = %q, want %q", got, want)
	}
	if out.Attribution == nil {
		t.Fatalf("Attribution = nil, want the week rollup")
	}
	if got, want := out.Attribution.WeekPct, 12.5; got != want {
		t.Errorf("Attribution.WeekPct = %v, want %v", got, want)
	}
}
