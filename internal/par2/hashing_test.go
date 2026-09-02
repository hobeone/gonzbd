package par2

import (
	"bytes"
	"crypto/md5" //nolint:gosec // par2 records MD5; matching it is the point
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// errReader fails after handing back a prefix, so a genuine read error can be
// told apart from the two end-of-input conditions that are successes.
type errReader struct {
	prefix []byte
	err    error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.prefix) > 0 {
		n := copy(p, r.prefix)
		r.prefix = r.prefix[n:]
		return n, nil
	}
	return 0, r.err
}

// TestHash16kOfReader pins the three ways io.ReadFull can end, because two of
// them are successes and one is not — and the distinction is the whole reason
// this helper exists as a single owner rather than as two copies inside
// ComputeHash16k and ComputeHash16kRoot.
//
// io.ReadFull returns io.ErrUnexpectedEOF when it read SOME of the buffer and
// io.EOF when it read NONE. Accepting only the former made a 0-byte file fail
// to hash at all, so Identify logged "could not hash candidate", skipped it,
// and left a par2 entry for an empty file permanently unaccounted.
func TestHash16kOfReader(t *testing.T) {
	t.Parallel()

	full := payload(5, 32*1024)

	for _, tc := range []struct {
		name string
		r    io.Reader
		want [16]byte
		fail bool
	}{
		{
			// io.EOF: nothing to read at all.
			name: "empty input hashes to the MD5 of no bytes",
			r:    bytes.NewReader(nil),
			want: md5.Sum(nil), //nolint:gosec // par2 compatibility, not security
		},
		{
			// io.ErrUnexpectedEOF: fewer than 16 KB available.
			name: "short input hashes what is there",
			r:    bytes.NewReader(full[:1000]),
			want: md5.Sum(full[:1000]), //nolint:gosec // par2 compatibility, not security
		},
		{
			// No error: more than 16 KB available, only the first 16 KB used.
			name: "long input hashes only the first 16 KB",
			r:    bytes.NewReader(full),
			want: md5.Sum(full[:16*1024]), //nolint:gosec // par2 compatibility, not security
		},
		{
			// A real failure must still be one. Treating every error as a
			// short read would hash a truncated prefix and call it the file.
			name: "a genuine read error is not a short read",
			r:    &errReader{prefix: full[:100], err: errors.New("disk on fire")},
			fail: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := hash16kOfReader(tc.r)
			if tc.fail {
				if err == nil {
					t.Fatal("hash16kOfReader returned nil error for a failing reader; a partial read would be " +
						"hashed and reported as the file's identity")
				}
				return
			}
			if err != nil {
				t.Fatalf("hash16kOfReader: %v", err)
			}
			if got != tc.want {
				t.Errorf("hash = %x, want %x", got, tc.want)
			}
		})
	}
}

// TestComputeHash16kRoot pins the os.Root variant against the path-based one.
//
// The two exist because callers differ in how they are allowed to open a file,
// not in what "the Hash16k of this content" means. They must therefore agree
// on every input, including the empty one — they share hash16kOfReader
// precisely so they cannot drift apart, and this is what would catch it if
// they were ever re-split.
func TestComputeHash16kRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	writeFile(t, dir, "big.bin", payload(6, 40*1024))
	writeFile(t, dir, "small.bin", payload(7, 500))
	writeFile(t, dir, "empty.bin", nil)

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer root.Close() //nolint:errcheck // read-only

	for _, name := range []string{"big.bin", "small.bin", "empty.bin"} {
		viaPath, pErr := ComputeHash16k(filepath.Join(dir, name))
		if pErr != nil {
			t.Fatalf("ComputeHash16k(%s): %v", name, pErr)
		}
		viaRoot, rErr := ComputeHash16kRoot(root, name)
		if rErr != nil {
			t.Fatalf("ComputeHash16kRoot(%s): %v", name, rErr)
		}
		if viaPath != viaRoot {
			t.Errorf("%s: path form = %x, root form = %x; the two must answer the same question",
				name, viaPath, viaRoot)
		}
	}

	// A missing file is still an error through the root form.
	if _, err := ComputeHash16kRoot(root, "absent.bin"); err == nil {
		t.Error("ComputeHash16kRoot returned nil error for a file that does not exist")
	}
}
