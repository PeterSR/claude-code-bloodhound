package aggregate

import "time"

// tryParse accepts a few common ISO shapes and returns (time, ok).
// store.RecordUsage writes resets in time.RFC3339, so that's the primary;
// time.Parse is forgiving enough that nanos versions work too.
func tryParse(s string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
