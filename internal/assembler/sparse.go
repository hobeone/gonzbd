package assembler

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SupportsSparse probes whether the filesystem at dir supports sparse
// files. It creates a temporary file, truncates it to 1 MiB, and
// checks whether the filesystem allocated proportionally fewer blocks
// than the apparent size (the hallmark of a sparse file).
//
// Returns true if sparse files work, false if they do not or if the
// check cannot be performed (e.g. permission denied). The probe file
// is cleaned up before returning.
func SupportsSparse(dir string) bool {
	const probeSize int64 = 1 << 20 // 1 MiB

	tmp, err := os.CreateTemp(dir, ".gonzbd-sparse-probe-*")
	if err != nil {
		return false // can't create files → can't test
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()

	if err := tmp.Truncate(probeSize); err != nil {
		return false
	}

	var stat syscall.Stat_t
	if err := syscall.Fstat(int(tmp.Fd()), &stat); err != nil {
		return false
	}

	// On sparse-capable filesystems, st_blocks * 512 will be much
	// less than the apparent file size because no data blocks were
	// allocated for the hole.
	allocatedBytes := stat.Blocks * 512
	return allocatedBytes < probeSize
}

// CheckSparseSupport tests sparse file support for the given directory
// and returns a human-readable status message.
func CheckSparseSupport(dir string) (supported bool, msg string) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path %q: %v", dir, err)
	}
	supported = SupportsSparse(absDir)
	if supported {
		msg = fmt.Sprintf("sparse file support: yes (%s)", absDir)
	} else {
		msg = fmt.Sprintf("sparse file support: no (%s) — file pre-allocation may consume extra disk space", absDir)
	}
	return supported, msg
}
