//go:build !linux

package cmdutil

import "fmt"

func wrapSandbox(targetDir, name string, args []string) (bin string, wrappedArgs []string, err error) {
	return "", nil, fmt.Errorf("%w", ErrSandboxUnsupported)
}
