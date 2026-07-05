//go:build linux

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	if _, err := lookPath("bwrap"); err != nil {
		return "", nil, fmt.Errorf("%w: bwrap binary not found in PATH", ErrSandboxUnavailable)
	}
	bwrapArgs := make([]string, 0, 14+len(args))
	bwrapArgs = append(bwrapArgs,
		"--ro-bind", "/", "/",
		"--bind", targetDir, targetDir,
		// --ro-bind of "/" is not recursive, so submounts like /proc, /dev and
		// /sys are NOT propagated into the sandbox namespace and would appear
		// as empty directories. Mount a fresh proc and a minimal dev so that
		// dynamically-linked extractors (unrar, 7z) can read /proc/self/* and
		// /dev/urandom instead of failing on hosts where those code paths fire.
		"--proc", "/proc",
		"--dev", "/dev",
		"--unshare-all",
		"--die-with-parent",
		"--",
		name,
	)
	bwrapArgs = append(bwrapArgs, args...)
	return "bwrap", bwrapArgs, nil
}
