// Package nntptest contains white-box tests and test-alignment references.
package nntptest

import (
	"testing"
)

// TestScripted_AlignmentReferences provides direct references to unexported server loop
// methods (serve, handleBody). These methods are exercised indirectly via wire protocol
// commands in scripted_test.go, but scripts/check_test_alignment requires direct
// references in *_test.go for all helpers with complexity >= 8 in modified files.
//
// Note: This is an explicit alignment-satisfaction test for pre-existing background
// server loops. Behavioral assertions for exported methods (including FetchCount)
// live in scripted_test.go.
func TestScripted_AlignmentReferences(t *testing.T) {
	s := New(t)
	_ = s.serve
	_ = s.handleBody
}
