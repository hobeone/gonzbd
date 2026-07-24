package nntptest

import (
	"testing"
)

func TestScripted_InternalMethods(t *testing.T) {
	s := New(t)
	// Verify internal methods can be referenced directly in white-box tests.
	_ = s.serve
	_ = s.handleBody
	_ = s.FetchCount("abc")
}
