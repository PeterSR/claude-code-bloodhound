package notify

import (
	"errors"
	"fmt"
	"testing"
)

func TestUndeliverableCoversTheQuietOutcomes(t *testing.T) {
	// The three that mean "nobody was listening" rather than "this is broken".
	// A caller notifying opportunistically branches on exactly this set, so a
	// new error escaping it silently turns a skip into a failure.
	for _, err := range []error{ErrNoSuchSession, ErrNoInbox, ErrUnreachable} {
		if !Undeliverable(err) {
			t.Errorf("Undeliverable(%v) = false, want true", err)
		}
		wrapped := fmt.Errorf("%w: session abc", err)
		if !Undeliverable(wrapped) {
			t.Errorf("Undeliverable(wrapped %v) = false, want true; Send wraps every one of these", err)
		}
	}

	for _, err := range []error{ErrEmptyText, errors.New("registry unreadable"), nil} {
		if Undeliverable(err) {
			t.Errorf("Undeliverable(%v) = true, want false", err)
		}
	}
}

func TestTargetString(t *testing.T) {
	tests := []struct {
		name string
		tgt  Target
		want string
	}{
		{"session", Target{SessionID: "abc"}, "session abc"},
		{"name", Target{Name: "myapp"}, "name myapp"},
		{"pid", Target{PID: 4242}, "pid 4242"},
		{"empty", Target{}, "no target"},
		// SessionID wins when several are set: it is the only one safe against
		// a recycled PID, so it is what Resolve keys on too.
		{"session wins", Target{SessionID: "abc", Name: "myapp", PID: 1}, "session abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tgt.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSendRefusesEmptyTextBeforeTouchingTheRegistry(t *testing.T) {
	// A frame with empty content is silently dropped by the receiver, so an
	// empty send would look like success and deliver nothing. Refuse it here,
	// and refuse it before the registry read so the check holds on a machine
	// with no sessions at all.
	for _, text := range []string{"", "   ", "\n\t "} {
		if _, err := New().Send(t.Context(), Target{SessionID: "abc"}, text); !errors.Is(err, ErrEmptyText) {
			t.Errorf("Send(%q) error = %v, want ErrEmptyText", text, err)
		}
	}
}

func TestResolveWithNoTargetSaysSo(t *testing.T) {
	_, err := New().Resolve(Target{})
	if !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("Resolve(empty) error = %v, want ErrNoSuchSession", err)
	}
}

func TestZeroSenderIsUsable(t *testing.T) {
	// Documented in the type comment; a nil client would panic on first use.
	var s Sender
	if _, err := s.Send(t.Context(), Target{SessionID: "abc"}, ""); !errors.Is(err, ErrEmptyText) {
		t.Errorf("zero Sender error = %v, want ErrEmptyText (not a nil-client panic)", err)
	}
}
