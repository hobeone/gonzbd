package par2

import "testing"

func TestCapsArgs_NilCaps(t *testing.T) {
	t.Parallel()
	args := capsArgs(nil, "/tmp/job")
	if len(args) != 0 {
		t.Errorf("expected nil/empty args for nil caps, got %v", args)
	}
}

func TestCapsArgs_NoDataSkippingOnly(t *testing.T) {
	t.Parallel()
	caps := &Caps{SupportsNoDataSkipping: true}
	args := capsArgs(caps, "/tmp/job")
	if len(args) != 1 || args[0] != "-N" {
		t.Errorf("expected [-N], got %v", args)
	}
}

func TestCapsArgs_BasepathOnly(t *testing.T) {
	t.Parallel()
	caps := &Caps{SupportsBasepath: true}
	args := capsArgs(caps, "/tmp/job")
	if len(args) != 2 || args[0] != "-B" || args[1] != "/tmp/job" {
		t.Errorf("expected [-B /tmp/job], got %v", args)
	}
}

func TestCapsArgs_Both(t *testing.T) {
	t.Parallel()
	caps := &Caps{SupportsNoDataSkipping: true, SupportsBasepath: true}
	args := capsArgs(caps, "/data")
	want := []string{"-N", "-B", "/data"}
	if len(args) != len(want) {
		t.Fatalf("expected %v, got %v", want, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestCapsArgs_BasepathEmptyDir(t *testing.T) {
	t.Parallel()
	caps := &Caps{SupportsBasepath: true}
	args := capsArgs(caps, "")
	if len(args) != 0 {
		t.Errorf("expected no -B for empty dir, got %v", args)
	}
}

func TestCapsArgs_NoFlags(t *testing.T) {
	t.Parallel()
	caps := &Caps{IsTurbo: true, Version: "0.9.0"}
	args := capsArgs(caps, "/tmp")
	if len(args) != 0 {
		t.Errorf("expected no args when neither -N nor -B supported, got %v", args)
	}
}
