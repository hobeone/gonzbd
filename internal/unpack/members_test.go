package unpack

import (
	"os"
	"path/filepath"
	"testing"
)

func par2LayoutFixture(layout, name string) string {
	return filepath.Join("..", "..", "test", "fixtures", "par2", layout, name)
}

func TestMemberBaseNames(t *testing.T) {
	t.Parallel()

	want := map[string]bool{"feature.bin": true, "absent.bin": true}
	cases := []struct {
		name string
		a    Archive
	}{
		{"rar", Archive{Type: RarArchive, MainFile: par2LayoutFixture("layout_b", "release.rar")}},
		{"7z", Archive{Type: SevenZipArchive, MainFile: par2LayoutFixture("layout_b", "release.7z")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			found, err := MemberBaseNames(tc.a, want)
			if err != nil {
				t.Fatalf("MemberBaseNames: %v", err)
			}
			if !found["feature.bin"] || found["absent.bin"] || len(found) != 1 {
				t.Errorf("found = %v, want only feature.bin", found)
			}
		})
	}
}

// Members below a directory are matched by base name, which is what par2
// entries are compared by.
func TestRarMemberBaseNames_NestedMember(t *testing.T) {
	t.Parallel()

	found, err := rarMemberBaseNames(filepath.Join("testdata", "single_rar5.rar"),
		map[string]bool{"nested.txt": true, "file1.txt": true})
	if err != nil {
		t.Fatalf("rarMemberBaseNames: %v", err)
	}
	if !found["nested.txt"] || !found["file1.txt"] {
		t.Errorf("found = %v, want nested.txt and file1.txt", found)
	}
}

func TestMemberBaseNames_Errors(t *testing.T) {
	t.Parallel()

	junk := filepath.Join(t.TempDir(), "junk.rar")
	if err := os.WriteFile(junk, []byte("not an archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"feature.bin": true}
	if _, err := rarMemberBaseNames(junk, want); err == nil {
		t.Error("rarMemberBaseNames on junk: want an error")
	}
	if _, err := rarMemberBaseNames(filepath.Join(t.TempDir(), "missing.rar"), want); err == nil {
		t.Error("rarMemberBaseNames on a missing file: want an error")
	}
	if _, err := sevenZipMemberBaseNames(junk, want); err == nil {
		t.Error("sevenZipMemberBaseNames on junk: want an error")
	}
	if _, err := MemberBaseNames(Archive{Type: SplitArchive, MainFile: junk}, want); err == nil {
		t.Error("MemberBaseNames on a split archive: want an error")
	}
}

func TestMemberBaseName(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"feature.bin":           "feature.bin",
		"Movie/feature.bin":     "feature.bin",
		`Movie\Sub\feature.bin`: "feature.bin",
		"Real.Name.part01.rar":  "Real.Name.part01.rar",
	} {
		if got := MemberBaseName(in); got != want {
			t.Errorf("MemberBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}
