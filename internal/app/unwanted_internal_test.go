package app

import "testing"

// TestUnwantedFailMessage pins the message's shape: SABnzbd's prefix, then
// at most three names, then a count of the rest.
func TestUnwantedFailMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		names []string
		want  string
	}{
		{[]string{"a.exe"}, "Aborted, unwanted extension detected: a.exe"},
		{[]string{"a.exe", "b.scr", "c.bat"}, "Aborted, unwanted extension detected: a.exe, b.scr, c.bat"},
		{[]string{"a.exe", "b.scr", "c.bat", "d.cmd", "e.msi"}, "Aborted, unwanted extension detected: a.exe, b.scr, c.bat and 2 more"},
	}
	for _, c := range cases {
		if got := unwantedFailMessage(c.names); got != c.want {
			t.Errorf("unwantedFailMessage(%q) = %q, want %q", c.names, got, c.want)
		}
	}
}
