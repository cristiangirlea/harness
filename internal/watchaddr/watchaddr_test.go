package watchaddr

import "testing"

func TestLogName(t *testing.T) {
	for _, tc := range []struct{ port, want string }{
		{"8089", "watch-8089.log"},
		{"custom", "watch-custom.log"},
		{"", "watch-.log"},
	} {
		if got := LogName(tc.port); got != tc.want {
			t.Errorf("LogName(%q) = %q, want %q", tc.port, got, tc.want)
		}
	}
}
