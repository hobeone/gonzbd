package notifier

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

func TestIsETXTBSY(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{syscall.ETXTBSY, true},
		{fmt.Errorf("wrapped error: %w", syscall.ETXTBSY), true},
		{errors.New("some other error"), false},
	}

	for i, tc := range tests {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			if got := isETXTBSY(tc.err); got != tc.want {
				t.Errorf("isETXTBSY(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
