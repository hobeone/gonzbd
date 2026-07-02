//go:build freebsd

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	if _, err := lookPath("jail"); err != nil {
		return "", nil, fmt.Errorf("%w: jail utility not found in PATH", ErrSandboxUnavailable)
	}
	jailArgs := make([]string, 0, 7+len(args))
	jailArgs = append(jailArgs,
		"-c",
		fmt.Sprintf("path=%s", targetDir),
		"mount.devfs",
		"ip4=disable",
		"ip6=disable",
		"persist=false",
		"command="+name,
	)
	jailArgs = append(jailArgs, args...)
	return "jail", jailArgs, nil
}
