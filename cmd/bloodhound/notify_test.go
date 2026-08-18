package main

import (
	"strings"
	"testing"
)

// resetNotifyFlags clears the package-level flag state between subtests, since
// cobra binds these once and they persist across calls.
func resetNotifyFlags() {
	notifySession, notifyName, notifyPID, notifySelf = "", "", 0, false
}

func TestNotifyTargetRequiresExactlyOne(t *testing.T) {
	t.Setenv(claudeCodeSessionEnvVar, "env-session-uuid")

	tests := []struct {
		name    string
		setup   func()
		want    string
		wantErr string
	}{
		{"session", func() { notifySession = "abc" }, "session abc", ""},
		{"name", func() { notifyName = "myapp" }, "name myapp", ""},
		{"pid", func() { notifyPID = 42 }, "pid 42", ""},
		{"self reads the env var", func() { notifySelf = true }, "session env-session-uuid", ""},
		{"none", func() {}, "", "pick a target"},
		{"two", func() { notifySession, notifyName = "abc", "myapp" }, "", "exactly one"},
		{"self plus explicit", func() { notifySelf, notifySession = true, "abc" }, "", "exactly one"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetNotifyFlags()
			tc.setup()
			got, err := notifyTarget()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("target = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestNotifySelfWithoutTheEnvVarExplainsWhy(t *testing.T) {
	// Running outside a Claude Code session is the normal case for a cron job
	// or a bare shell, and "no target" would be a confusing way to say it.
	t.Setenv(claudeCodeSessionEnvVar, "")
	resetNotifyFlags()
	notifySelf = true
	_, err := notifyTarget()
	if err == nil || !strings.Contains(err.Error(), claudeCodeSessionEnvVar) {
		t.Fatalf("error = %v, want one naming %s", err, claudeCodeSessionEnvVar)
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("error = %v, want it to point at the --session workaround", err)
	}
}

func TestNotifyTextPrefersTheArgument(t *testing.T) {
	got, err := notifyText(strings.NewReader("from stdin"), []string{"from arg"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "from arg" {
		t.Errorf("text = %q, want %q", got, "from arg")
	}
}

func TestNotifyTextFallsBackToStdin(t *testing.T) {
	// The trailing newline matters: `echo x | bloodhound notify` is the common
	// shape and the newline is the shell's, not part of the message.
	got, err := notifyText(strings.NewReader("piped in\n"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "piped in" {
		t.Errorf("text = %q, want %q (trailing newline stripped)", got, "piped in")
	}
}

func TestNotifyTextKeepsInteriorNewlines(t *testing.T) {
	got, err := notifyText(strings.NewReader("line one\nline two\n"), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "line one\nline two" {
		t.Errorf("text = %q, want the interior newline preserved", got)
	}
}

func TestNotifyTextRejectsNothingToSay(t *testing.T) {
	for _, in := range []string{"", "\n", "   \n\t"} {
		if _, err := notifyText(strings.NewReader(in), nil); err == nil {
			t.Errorf("notifyText(%q) succeeded, want an error", in)
		}
	}
}
