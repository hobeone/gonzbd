package nntp

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// --- C1: validateMessageID ---

func TestValidateMessageID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ids     []string
		wantErr bool
	}{
		{
			name: "crlf injection",
			ids: []string{
				"abc\r\nQUIT\r\n@news.example.com",
				"abc\n@news.example.com",
				"abc\r@news.example.com",
			},
			wantErr: true,
		},
		{
			name:    "null byte",
			ids:     []string{"abc\x00def@news.example.com"},
			wantErr: true,
		},
		{
			name:    "empty and brackets",
			ids:     []string{"", "<>", "<", ">"},
			wantErr: true,
		},
		{
			name:    "close angle bracket injection",
			ids:     []string{"abc>QUIT@news.example.com"},
			wantErr: true,
		},
		{
			name: "valid normal ids",
			ids: []string{
				"abc123@news.example.com",
				"<abc123@news.example.com>",
				"part1of100.abc@provider.net",
				"a",
			},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, id := range tc.ids {
				err := validateMessageID(id)
				if tc.wantErr {
					if err == nil {
						t.Errorf("validateMessageID(%q) = nil, want error", id)
					} else if !errors.Is(err, ErrInvalidMessageID) {
						t.Errorf("validateMessageID(%q) = %v, want ErrInvalidMessageID", id, err)
					}
				} else if err != nil {
					t.Errorf("validateMessageID(%q) = %v, want nil", id, err)
				}
			}
		})
	}
}

// --- C2: readResponseLine ---

func TestReadResponseLine_MaxLength(t *testing.T) {
	// Build a 4000-char string with no newline. Use a small bufio
	// buffer so ReadSlice hits ErrBufferFull before EOF.
	huge := strings.Repeat("A", 4000)
	br := bufio.NewReaderSize(strings.NewReader(huge), 256)
	_, err := readResponseLine(br)
	if err == nil {
		t.Fatal("readResponseLine accepted 4000-byte line without newline")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadResponseLine_MaxLengthWithNewline(t *testing.T) {
	// A 3000-byte line terminated by \n should still be rejected.
	huge := strings.Repeat("B", 3000) + "\n"
	br := bufio.NewReader(strings.NewReader(huge))
	_, err := readResponseLine(br)
	if err == nil {
		t.Fatal("readResponseLine accepted 3000-byte terminated line")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadResponseLine_Normal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"200 OK\r\n", "200 OK"},
		{"200 OK\n", "200 OK"},
		{"200\r\n", "200"},
	}
	for _, tc := range tests {
		br := bufio.NewReader(strings.NewReader(tc.input))
		got, err := readResponseLine(br)
		if err != nil {
			t.Errorf("readResponseLine(%q) error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("readResponseLine(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
