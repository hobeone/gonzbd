package unpack

import "strings"

// formatArgs returns a copy of the argument slice with any password flags matching
// redact replaced by "-p<redacted>" to avoid leaking passwords in logs and UI output.
func formatArgs(args []string, redact string) []string {
	if args == nil {
		return nil
	}
	safe := make([]string, 0, len(args))
	for _, arg := range args {
		if redact != "" && arg == redact && strings.HasPrefix(arg, "-p") && arg != "-p-" {
			safe = append(safe, "-p<redacted>")
		} else {
			safe = append(safe, arg)
		}
	}
	return safe
}

// formatCmdLine builds a display-safe command line string from a binary path
// and its arguments. If redact is non-empty and appears in args, it is replaced
// with the redacted form (e.g. "-p<redacted>") to avoid leaking passwords in
// logs and UI output.
func formatCmdLine(bin string, args []string, redact string) string {
	safe := make([]string, 0, 1+len(args))
	safe = append(safe, bin)
	safe = append(safe, formatArgs(args, redact)...)
	return strings.Join(safe, " ")
}
