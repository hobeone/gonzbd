package cmdutil

import (
	"errors"
	"testing"
)

func TestSafeEngineRun(t *testing.T) {
	t.Parallel()

	t.Run("normal execution returns nil", func(t *testing.T) {
		t.Parallel()
		called := false
		err := SafeEngineRun("test_prefix", func() error {
			called = true
			return nil
		})
		if err != nil {
			t.Fatalf("expected nil error, got %v", err)
		}
		if !called {
			t.Fatal("expected function to be called")
		}
	})

	t.Run("normal execution returns error", func(t *testing.T) {
		t.Parallel()
		expected := errors.New("normal error")
		err := SafeEngineRun("test_prefix", func() error {
			return expected
		})
		if !errors.Is(err, expected) {
			t.Fatalf("expected %v, got %v", expected, err)
		}
	})

	t.Run("panic with string is trapped and converted to error", func(t *testing.T) {
		t.Parallel()
		var panicVal any
		callbackCalled := false

		err := SafeEngineRun("test_prefix", func() error {
			panic("engine exploded")
		}, func(p any) {
			panicVal = p
			callbackCalled = true
		})

		if err == nil {
			t.Fatal("expected error from panic, got nil")
		}
		expectedMsg := "test_prefix: engine exploded"
		if err.Error() != expectedMsg {
			t.Fatalf("expected error %q, got %q", expectedMsg, err.Error())
		}
		if !callbackCalled {
			t.Fatal("expected onPanic callback to be called")
		}
		if panicVal != "engine exploded" {
			t.Fatalf("expected panic value %q, got %v", "engine exploded", panicVal)
		}
	})

	t.Run("panic with error is trapped and converted to error", func(t *testing.T) {
		t.Parallel()
		panicErr := errors.New("internal library panic")
		err := SafeEngineRun("test_prefix", func() error {
			panic(panicErr)
		})

		if err == nil {
			t.Fatal("expected error from panic, got nil")
		}
		expectedMsg := "test_prefix: internal library panic"
		if err.Error() != expectedMsg {
			t.Fatalf("expected error %q, got %q", expectedMsg, err.Error())
		}
		if !errors.Is(err, panicErr) {
			t.Fatalf("expected error to wrap %v so errors.Is works, but got %v", panicErr, err)
		}
	})

	t.Run("multiple panic callbacks are called", func(t *testing.T) {
		t.Parallel()
		call1 := false
		call2 := false

		err := SafeEngineRun("test_prefix", func() error {
			panic("boom")
		}, func(_ any) {
			call1 = true
		}, nil, func(_ any) {
			call2 = true
		})

		if err == nil {
			t.Fatal("expected error from panic, got nil")
		}
		if !call1 || !call2 {
			t.Fatalf("expected both callbacks to be called, got call1=%v, call2=%v", call1, call2)
		}
	})
}
