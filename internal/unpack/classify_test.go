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
