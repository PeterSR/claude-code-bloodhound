package usage

import "testing"

func TestTrustCursorOnYes(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   bool
	}{
		{"current layout opens on no", " Quick safety check: Is this a project you created or one you trust?\n ❯ No, exit\n   Yes, I trust this folder\n", false},
		{"current layout after moving down", " Quick safety check\n   No, exit\n ❯ Yes, I trust this folder\n", true},
		{"older numbered layout", " Do you trust the files in this folder?\n ❯ 1. Yes, proceed\n   2. No, exit\n", true},
		{"no cursor at all", " Accessing workspace:\n", false},
	}
	for _, c := range cases {
		if got := TrustCursorOnYes(c.screen); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
