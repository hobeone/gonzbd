package rarheader

import (
	"path/filepath"
	"slices"
	"testing"
)

// InspectNative lists what a volume starts, wherever it sits in its set.
// late_member.part3.rar opens with the tail of first.bin and then starts
// second.bin; an extraction Reader refuses it as not the first volume.
func TestInspectNative_LaterVolumeListsTheMemberItStarts(t *testing.T) {
	t.Parallel()

	info, err := InspectNative(filepath.Join("..", "unpack", "testdata", "late_member.part3.rar"))
	if err != nil {
		t.Fatalf("InspectNative: %v", err)
	}
	if !slices.Equal(info.Filenames, []string{"second.bin"}) {
		t.Errorf("Filenames = %q, want [second.bin]", info.Filenames)
	}
}

// rar4_refused_member.rar declares bad.bin with an unknown method byte, which
// rarengine refuses by name, and then a stored good.bin. The refusal is that
// member's verdict, not the listing's: both names are reported.
func TestInspectNative_ListsAMemberRefusedByName(t *testing.T) {
	t.Parallel()

	info, err := InspectNative(filepath.Join("..", "unpack", "testdata", "rar4_refused_member.rar"))
	if err != nil {
		t.Fatalf("InspectNative: %v", err)
	}
	if !slices.Equal(info.Filenames, []string{"bad.bin", "good.bin"}) {
		t.Errorf("Filenames = %q, want [bad.bin good.bin]", info.Filenames)
	}
}
