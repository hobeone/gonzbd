package par2

import "strings"

// formatParCmdLine builds a display string from a par2 binary path and its
// arguments. Unlike the unpack package's helper, par2 commands have no secrets
// to redact, so this is a simple join.
func formatParCmdLine(bin string, args []string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, bin)
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}
