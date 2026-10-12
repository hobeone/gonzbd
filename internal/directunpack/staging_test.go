package directunpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/unpack"
)

// twoMembersVolumes is the volume count of testdata's two_members set:
// first.txt ends in volume 1, second.txt runs on to volume 6.
const twoMembersVolumes = 6

const twoMembersFirst = "first member, complete in volume 1\n"

func twoMembersVolume(n int) string { return fmt.Sprintf("two_members.part%d.rar", n) }

// twoMembersSecond is second.txt's content, as scripts/gen_test_archives.sh
// writes it.
func twoMembersSecond() string {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "second member line %04d\n", i)
	}
	return b.String()
}

// copyTwoMembers copies the two_members volumes into jobDir, which stands in
// for job.DownloadDir: DirectUnpack reads the volumes from it and extracts
// into it.
func copyTwoMembers(t *testing.T, jobDir string) []string {
	t.Helper()
	src := testdataDir(t)
	names := make([]string, 0, twoMembersVolumes)
	for i := 1; i <= twoMembersVolumes; i++ {
		copyRAR(t, src, jobDir, twoMembersVolume(i))
		names = append(names, twoMembersVolume(i))
	}
	return names
}

// damageVolume flips one byte in the middle of a volume, inside second.txt's
// stored data, so second.txt fails its checksum after first.txt has been
// extracted. It returns the original bytes, for a test that "repairs" the
// volume afterwards.
func damageVolume(t *testing.T, path string) []byte {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := slices.Clone(orig)
	bad[len(bad)/2] ^= 0xff
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	return orig
}

// runTwoMembers feeds every volume in jobDir to a DirectUnpacker extracting
// into jobDir and waits for it.
func runTwoMembers(t *testing.T, jobDir string, names []string, opts Options) *DirectUnpacker {
	t.Helper()
	du := New(testLogger(t), "test-job", jobDir, jobDir, opts)
	du.SetAllFilenames(names)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, n := range names {
		du.Add(ctx, n, filepath.Join(jobDir, n))
	}
	du.Wait()
	return du
}

// dirNames lists dir's entries, sorted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Errorf("%s = %.40q, %v; want %.40q", filepath.Base(path), b, err, want)
	}
}

// A set that fails on a later volume publishes nothing: first.txt was
// extracted and verified before second.txt failed, and it is not left in the
// job directory, nor is the staging directory it was written to.
func TestDirectUnpack_FailedSetLeavesNothingInTheJobDir(t *testing.T) {
	jobDir := t.TempDir()
	names := copyTwoMembers(t, jobDir)
	damageVolume(t, filepath.Join(jobDir, twoMembersVolume(3)))

	du := runTwoMembers(t, jobDir, names, Options{})

	if _, failed := du.Failures()["two_members"]; !failed {
		t.Fatalf("the damaged set did not fail: results %+v, failures %+v", du.Results(), du.Failures())
	}
	if got := dirNames(t, jobDir); !slices.Equal(got, names) {
		t.Errorf("job dir after a failed set = %q, want only the volumes %q", got, names)
	}
}

// An aborted set publishes nothing either, and its staging directory is
// removed.
func TestDirectUnpack_AbortedSetLeavesNothingInTheJobDir(t *testing.T) {
	jobDir := t.TempDir()
	names := copyTwoMembers(t, jobDir)

	firstDone := make(chan struct{})
	var once bool
	du := New(testLogger(t), "test-job", jobDir, jobDir, Options{OnLine: func(l string) {
		if l == "Extracting  first.txt" && !once {
			once = true
			close(firstDone)
		}
	}})
	du.SetAllFilenames(names)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Volumes 1 and 2 only: extraction waits for volume 3 inside second.txt.
	for _, n := range names[:2] {
		du.Add(ctx, n, filepath.Join(jobDir, n))
	}
	select {
	case <-firstDone:
	case <-ctx.Done():
		t.Fatal("first.txt was never extracted")
	}
	du.Abort()

	if got := dirNames(t, jobDir); !slices.Equal(got, names) {
		t.Errorf("job dir after an aborted set = %q, want only the volumes %q", got, names)
	}
}

// Under flat_unpack without overwrite_files, post-processing re-extracts a set
// DirectUnpack failed into the same directory. Nothing of the failed attempt is
// there to collide with, so no member comes out under a "_1" name.
func TestDirectUnpack_FailedSetReextractedFlatHasNoDuplicate(t *testing.T) {
	jobDir := t.TempDir()
	names := copyTwoMembers(t, jobDir)
	damaged := filepath.Join(jobDir, twoMembersVolume(3))
	orig := damageVolume(t, damaged)

	du := runTwoMembers(t, jobDir, names, Options{OneFolder: true})
	if _, failed := du.Failures()["two_members"]; !failed {
		t.Fatalf("the damaged set did not fail: failures %+v", du.Failures())
	}

	// par2 repair restores the volume; the unpack stage then extracts the set.
	if err := os.WriteFile(damaged, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	archives, err := unpack.Scan(jobDir)
	if err != nil || len(archives) != 1 {
		t.Fatalf("Scan = %+v, %v; want the one set", archives, err)
	}
	if _, err := unpack.GoUnRAR(context.Background(), testLogger(t), archives[0], jobDir, "", unpack.Options{OneFolder: true}); err != nil {
		t.Fatalf("GoUnRAR: %v", err)
	}

	want := append(slices.Clone(names), "first.txt", "second.txt")
	slices.Sort(want)
	if got := dirNames(t, jobDir); !slices.Equal(got, want) {
		t.Errorf("job dir after re-extraction = %q, want %q", got, want)
	}
	assertFile(t, filepath.Join(jobDir, "first.txt"), twoMembersFirst)
	assertFile(t, filepath.Join(jobDir, "second.txt"), twoMembersSecond())
}

// A set that succeeds has its members published into the job directory, under
// the names ExtractedFiles lists, and leaves no staging directory behind.
func TestDirectUnpack_SucceededSetIsPublishedToTheJobDir(t *testing.T) {
	jobDir := t.TempDir()
	names := copyTwoMembers(t, jobDir)

	du := runTwoMembers(t, jobDir, names, Options{})

	if f := du.Failures(); len(f) != 0 {
		t.Fatalf("failures = %+v, want none", f)
	}
	first, second := filepath.Join(jobDir, "first.txt"), filepath.Join(jobDir, "second.txt")
	assertFile(t, first, twoMembersFirst)
	assertFile(t, second, twoMembersSecond())
	got := slices.Sorted(slices.Values(du.Results()["two_members"].ExtractedFiles))
	if want := []string{first, second}; !slices.Equal(got, want) {
		t.Errorf("ExtractedFiles = %q, want %q", got, want)
	}
	want := append(slices.Clone(names), "first.txt", "second.txt")
	slices.Sort(want)
	if got := dirNames(t, jobDir); !slices.Equal(got, want) {
		t.Errorf("job dir = %q, want %q", got, want)
	}
}

// flat_unpack without overwrite_files renames a member whose name a file in
// the job directory already holds, as ExtractMember does within one set: an
// earlier set's member of the same name is not replaced, and this set's
// member is not dropped.
func TestDirectUnpack_FlatCollisionWithTheJobDirIsRenamed(t *testing.T) {
	jobDir := t.TempDir()
	names := copyTwoMembers(t, jobDir)
	const earlier = "an earlier set's first.txt"
	if err := os.WriteFile(filepath.Join(jobDir, "first.txt"), []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}

	du := runTwoMembers(t, jobDir, names, Options{OneFolder: true})

	if f := du.Failures(); len(f) != 0 {
		t.Fatalf("failures = %+v, want none", f)
	}
	assertFile(t, filepath.Join(jobDir, "first.txt"), earlier)
	assertFile(t, filepath.Join(jobDir, "first_1.txt"), twoMembersFirst)
	if files := du.Results()["two_members"].ExtractedFiles; !slices.Contains(files, filepath.Join(jobDir, "first_1.txt")) {
		t.Errorf("ExtractedFiles = %q, want it to list first_1.txt", files)
	}
}
