package unpack

import "strings"

// formatCmdLine builds a display-safe command line string from a binary path
// and its arguments. If redact is non-empty and appears in args, it is replaced
// with the redacted form (e.g. "-p<redacted>") to avoid leaking passwords in
// logs and UI output.
func formatCmdLine(bin string, args []string, redact string) string {
	safe := make([]string, 0, 1+len(args))
	safe = append(safe, bin)
	for _, arg := range args {
		if redact != "" && arg == redact && strings.HasPrefix(arg, "-p") && arg != "-p-" {
			safe = append(safe, "-p<redacted>")
		} else {
			safe = append(safe, arg)
		}
	}
	return strings.Join(safe, " ")
}
