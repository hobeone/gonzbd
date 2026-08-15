package unpack

import "testing"

func TestClassifyUnrarOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		want   FailReason
	}{
		{
			name:   "incorrect password",
			output: "Extracting  foo.zip     Incorrect password for foo.zip",
			want:   FailWrongPassword,
		},
		{
			name:   "specified password incorrect",
			output: "The specified password is incorrect.\n",
			want:   FailWrongPassword,
		},
		{
			name:   "checksum error encrypted",
			output: "foo.rar : Checksum error in the encrypted file\n",
			want:   FailWrongPassword,
		},
		{
			name:   "encrypted CRC failed",
			output: "foo.rar : Encrypted file:  CRC failed in bar.mkv\n",
			want:   FailWrongPassword,
		},
		{
			name:   "write error",
			output: "Write error in the file foo.mkv\n",
			want:   FailDiskFull,
		},
		{
			name:   "not enough space",
			output: "There is not enough space on the disk\n",
			want:   FailDiskFull,
		},
		{
			name:   "no space left linux",
			output: "unrar: write error: No space left on device\n",
			want:   FailDiskFull,
		},
		{
			name:   "not rar archive",
			output: "foo.zip is not RAR archive\nNo files to extract\n",
			want:   FailNotArchive,
		},
		{
			name:   "missing volume",
			output: "Cannot find volume part2.rar\n",
			want:   FailMissingVolume,
		},
		{
			name:   "missing volume during recovery ignored",
			output: "10 recovery volumes found\nCannot find volume part3.rar\n",
			want:   FailUnknown, // recovery mode suppresses missing vol
		},
		{
			name:   "missing volume during reconstruction ignored",
			output: "Cannot find volume part2.rar\nReconstructing part2.rar\n",
			want:   FailUnknown, // N3: reconstruction suppresses missing vol
		},
		{
			name:   "checksum error non-encrypted",
			output: "foo.mkv  checksum error\n",
			want:   FailCorrupt,
		},
		{
			name:   "unexpected end",
			output: "Unexpected end of archive\n",
			want:   FailCorrupt,
		},
		{
			name:   "CRC failed non-encrypted",
			output: "foo.mkv  - CRC failed\n",
			want:   FailCorrupt,
		},
		{
			name:   "generic error",
			output: "ERROR: some unknown issue\n",
			want:   FailUnknown,
		},
		{
			name:   "file too large",
			output: "file.mkv : File too large\n",
			want:   FailFileTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ClassifyUnrarOutput(tt.output)
			if got != tt.want {
				t.Errorf("ClassifyUnrarOutput() = %v (%s), want %v (%s)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestClassify7zOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		want   FailReason
	}{
		{
			name:   "wrong password",
			output: "Data Error in encrypted file. Wrong password?\n",
			want:   FailWrongPassword,
		},
		{
			name:   "cannot open encrypted",
			output: "Can not open encrypted archive. Wrong password?\n",
			want:   FailWrongPassword,
		},
		{
			name:   "disk full",
			output: "ERROR: Disk full.\n",
			want:   FailDiskFull,
		},
		{
			name:   "no space linux",
			output: "No space left on device\n",
			want:   FailDiskFull,
		},
		{
			name:   "CRC failed",
			output: "ERROR: CRC Failed : foo.mkv\n",
			want:   FailCorrupt,
		},
		{
			name:   "generic error",
			output: "Sub items Errors: 1\n",
			want:   FailUnknown,
		},
		{
			name:   "not an archive - cannot open",
			output: "ERROR: Cannot open the file as [7z] archive\n",
			want:   FailNotArchive,
		},
		{
			name:   "not an archive - unsupported",
			output: "ERROR: file.xyz is not supported archive\n",
			want:   FailNotArchive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Classify7zOutput(tt.output)
			if got != tt.want {
				t.Errorf("Classify7zOutput() = %v (%s), want %v (%s)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestFailReason_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		r    FailReason
		want string
	}{
		{FailUnknown, "unknown error"},
		{FailWrongPassword, "wrong password"},
		{FailDiskFull, "disk full"},
		{FailCorrupt, "corrupt archive"},
		{FailMissingVolume, "missing volume"},
		{FailNotArchive, "not an archive"},
		{FailFileTooLarge, "file too large"},
	}
	for _, tt := range tests {
		if got := tt.r.String(); got != tt.want {
			t.Errorf("FailReason(%d).String() = %q, want %q", tt.r, got, tt.want)
		}
	}
}

// TestIsUnrarPasswordError pins the password predicate directly, not only
// through ClassifyUnrarOutput.
//
// It is worth its own test because the predicate is where the *variants* live,
// and ClassifyUnrarOutput checks it first — so a false positive here does not
// produce a wrong-looking password verdict, it produces a wrong verdict for
// every other failure mode that happens to contain one of these substrings. The
// negative cases below are therefore the load-bearing half.
func TestIsUnrarPasswordError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lower string
		want  bool
	}{
		{"incorrect password", "extracting foo.rar     incorrect password for foo.rar", true},
		{"specified password is incorrect", "the specified password is incorrect.\n", true},
		{"checksum error in the encrypted file", "foo.rar : checksum error in the encrypted file\n", true},
		{"encrypted file with CRC failed, wide spacing", "foo.rar : encrypted file:  crc failed in bar.mkv\n", true},
		{"encrypted file and CRC failed on separate lines still matches", "encrypted file: bar.mkv\nsome other line\ncrc failed\n", true},

		// The negatives. Each holds one half of the two-part clause, or a
		// near-miss of a whole one; none may be read as a password failure.
		{"plain CRC failure is corruption, not a password", "foo.rar : crc failed in bar.mkv\n", false},
		{"an encrypted file that extracted fine", "encrypted file: bar.mkv\nall ok\n", false},
		{"a correct password mentioned in passing", "password accepted\n", false},
		{"empty output", "", false},
		{"unrelated failure", "cannot open foo.rar: no such file or directory\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isUnrarPasswordError(tc.lower); got != tc.want {
				t.Errorf("isUnrarPasswordError(%q) = %v, want %v", tc.lower, got, tc.want)
			}
		})
	}
}

// TestIsUnrarPasswordError_RequiresAlreadyLowercasedInput records the caller
// contract in an executable form. The function does no folding of its own —
// ClassifyUnrarOutput lowercases once for every predicate it consults — so a
// future caller passing raw output would silently get false for real password
// failures, which routes a bad-password extraction to FailUnknown.
func TestIsUnrarPasswordError_RequiresAlreadyLowercasedInput(t *testing.T) {
	t.Parallel()
	const raw = "The specified password is incorrect.\n"
	if isUnrarPasswordError(raw) {
		t.Fatal("isUnrarPasswordError matched mixed-case input; it is documented as taking " +
			"already-lowercased text, and a match here would mean the contract had changed silently")
	}
	if !isUnrarPasswordError("the specified password is incorrect.\n") {
		t.Fatal("the lowercased form must match")
	}
}
