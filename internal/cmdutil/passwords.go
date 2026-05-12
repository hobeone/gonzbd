package cmdutil

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadPasswordFile reads a text file containing one password per line.
// Blank lines and lines starting with # are skipped. Leading/trailing
// whitespace is trimmed. Returns nil, nil if path is empty.
func LoadPasswordFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open password file %q: %w", path, err)
	}
	defer f.Close()

	var passwords []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		passwords = append(passwords, line)
	}
	if err := scanner.Err(); err != nil {
		return passwords, fmt.Errorf("read password file %q: %w", path, err)
	}
	return passwords, nil
}
