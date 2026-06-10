package humanfmt

import (
	"testing"
	"time"
)

func TestBytes(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{-100, "-100 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1025, "1.00 KB"},
		{1536, "1.50 KB"},
		{(1 << 20) - 1, "1024.00 KB"},
		{1 << 20, "1.00 MB"},
		{(1 << 20) + 1024, "1.00 MB"},
		{(1 << 30) - 1, "1024.00 MB"},
		{1 << 30, "1.00 GB"},
		{(1 << 30) + (1 << 20), "1.00 GB"},
		{(1 << 40) - 1, "1024.00 GB"},
		{1 << 40, "1.00 TB"},
		{(1 << 40) + (1 << 30), "1.00 TB"},
		{5 * (1 << 40), "5.00 TB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := Bytes(tc.input)
			if got != tc.want {
				t.Errorf("Bytes(%d) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestBytesSI(t *testing.T) {
	cases := []struct {
		input int64
		want  string
	}{
		{0, "0 B"},
		{-5, "-5 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536 * 1024, "1.5 MiB"},
		{1 << 30, "1.0 GiB"},
		// TiB and above index into the "KMGTPE" suffix table. The original
		// "KMG" table panicked (index out of range) at TiB (exp=3); these
		// cases pin every suffix the fix unlocked, up to EiB (exp=5), which
		// is the largest scale reachable by int64 (max ~8 EiB).
		{1 << 40, "1.0 TiB"},
		{1 << 50, "1.0 PiB"},
		{1 << 60, "1.0 EiB"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := BytesSI(tc.input)
			if got != tc.want {
				t.Errorf("BytesSI(%d) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		input time.Duration
		want  string
	}{
		{0, "0.0s"},
		{-10 * time.Second, "-10.0s"},
		{45 * time.Second, "45.0s"},
		{59 * time.Second, "59.0s"},
		{60 * time.Second, "1m 0s"},
		{61 * time.Second, "1m 1s"},
		{5*time.Minute + 30*time.Second, "5m 30s"},
		{59*time.Minute + 59*time.Second, "59m 59s"},
		{60 * time.Minute, "1h 0m"},
		{61 * time.Minute, "1h 1m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := Duration(tc.input)
			if got != tc.want {
				t.Errorf("Duration(%v) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
