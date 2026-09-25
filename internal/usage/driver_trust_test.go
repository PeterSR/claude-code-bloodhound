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

func TestHasInputPrompt(t *testing.T) {
	cases := []struct {
		name   string
		screen string
		want   bool
	}{
		{"bare prompt", "────\n❯ \n────\n", true},
		{"placeholder, ascii space", "────\n❯ Try \"refactor <filepath>\"\n────\n", true},
		{"placeholder, nbsp (2.1.282)", "────\n❯\u00a0Try \"create a util logging.py that...\"\n────\n", true},
		{"bare prompt, nbsp", "────\n❯\u00a0\n────\n", true},
		{"trust menu row", " ❯ 1. Yes, I trust this folder\n   2. No, exit\n", false},
		{"typed input", "❯ /usage\n", false},
		{"no gap before placeholder", "❯Try \"x\"\n", false},
	}
	for _, c := range cases {
		if got := HasInputPrompt(c.screen); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
