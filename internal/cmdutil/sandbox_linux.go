//go:build linux

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	if _, err := lookPath("bwrap"); err != nil {
		return "", nil, fmt.Errorf("%w: bwrap binary not found in PATH", ErrSandboxUnavailable)
	}
	bwrapArgs := make([]string, 0, 10+len(args))
	bwrapArgs = append(bwrapArgs,
		"--ro-bind", "/", "/",
		"--bind", targetDir, targetDir,
		"--unshare-all",
		"--die-with-parent",
		"--",
		name,
	)
	bwrapArgs = append(bwrapArgs, args...)
	return "bwrap", bwrapArgs, nil
}
