//go:build darwin

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	if _, err := lookPath("sandbox-exec"); err != nil {
		return "", nil, fmt.Errorf("%w: sandbox-exec binary not found in PATH", ErrSandboxUnavailable)
	}
	profile := fmt.Sprintf(`(version 1) (allow default) (deny file-write*) (allow file-write* (subpath "%s"))`, targetDir)
	seArgs := make([]string, 0, 3+len(args))
	seArgs = append(seArgs, "-p", profile, name)
	seArgs = append(seArgs, args...)
	return "sandbox-exec", seArgs, nil
}
