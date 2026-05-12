package unpack

import (
	"testing"
)

func TestAllPasswords_DeduplicatesAndOrders(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "empty",
			opts: Options{},
			want: nil,
		},
		{
			name: "single_password_only",
			opts: Options{Password: "secret"},
			want: []string{"secret"},
		},
		{
			name: "passwords_list_only",
			opts: Options{Passwords: []string{"a", "b", "c"}},
			want: []string{"a", "b", "c"},
		},
		{
			name: "both_no_overlap",
			opts: Options{Password: "fallback", Passwords: []string{"first", "second"}},
			want: []string{"first", "second", "fallback"},
		},
		{
			name: "both_with_overlap",
			opts: Options{Password: "first", Passwords: []string{"first", "second"}},
			want: []string{"first", "second"},
		},
		{
			name: "empty_strings_filtered",
			opts: Options{Password: "", Passwords: []string{"", "real", ""}},
			want: []string{"real"},
		},
		{
			name: "duplicates_in_list",
			opts: Options{Passwords: []string{"a", "b", "a", "c", "b"}},
			want: []string{"a", "b", "c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := allPasswords(tc.opts)
			if len(got) != len(tc.want) {
				t.Fatalf("allPasswords() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("allPasswords()[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestIsUnrarWrongPassword(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		output   string
		want     bool
	}{
		{"incorrect_password", 2, "Incorrect password for file.rar", true},
		{"specified_password", 2, "The specified password is incorrect.", true},
		{"checksum_encrypted", 2, "Checksum error in the encrypted file foo.dat", true},
		{"crc_failed_encrypted", 2, "Encrypted file: CRC failed in data.bin", true},
		{"normal_crc_error", 3, "CRC failed in data.bin", false},
		{"success", 0, "All OK", false},
		{"other_error", 7, "Cannot create output/path", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isUnrarWrongPassword(tc.exitCode, tc.output)
			if got != tc.want {
				t.Errorf("isUnrarWrongPassword(%d, %q) = %v, want %v",
					tc.exitCode, tc.output, got, tc.want)
			}
		})
	}
}

func TestIs7zWrongPassword(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		output   string
		want     bool
	}{
		{"wrong_password_question", 2, "Data Error in encrypted file. Wrong password?", true},
		{"wrong_password_short", 2, "Wrong password?", true},
		{"data_error_encrypted", 2, "Data Error in encrypted file foo.7z", true},
		{"normal_data_error", 2, "Data error in file.7z", false},
		{"success", 0, "Everything is Ok", false},
		{"other_error", 7, "Cannot open archive", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := is7zWrongPassword(tc.exitCode, tc.output)
			if got != tc.want {
				t.Errorf("is7zWrongPassword(%d, %q) = %v, want %v",
					tc.exitCode, tc.output, got, tc.want)
			}
		})
	}
}
