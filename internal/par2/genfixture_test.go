package par2

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateSubdirFixture regenerates test/fixtures/par2/subdir.par2, a set
// whose single entry names a file inside a subdirectory. It is skipped unless
// GONZBD_REGEN_FIXTURES is set, so an ordinary run neither writes nor depends
// on it.
//
// The fixture exists because internal/app needs a subdirectory-relocating par2
// set and cannot reach this package's unexported packet builders. Generating
// it here rather than hand-rolling a third copy of the packet format keeps one
// writer of that format.
//
// The payload is test/fixtures/par2/data.bin, so the same file already used by
// the app tests is what this set protects.
func TestGenerateSubdirFixture(t *testing.T) {
	if os.Getenv("GONZBD_REGEN_FIXTURES") == "" {
		t.Skip("set GONZBD_REGEN_FIXTURES=1 to regenerate")
	}

	body, err := os.ReadFile("../../test/fixtures/par2/data.bin")
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}

	setID := [16]byte{3, 1, 4, 1, 5, 9}
	fileID := [16]byte{2, 7, 1, 8}
	content := buildPacket(setID, typeMain, buildMainBody(testSliceSize, fileID))
	content = append(content, buildPacket(setID, typeFileDesc,
		buildFileDescBody(fileID, [16]byte{0xD0}, hash16kOf(body), uint64(len(body)), "Screens/data.bin"))...)
	content = append(content, buildPacket(setID, typeIFSC, buildIFSCBody(fileID, ifscSlicesFor(body)))...)

	out := filepath.Join("..", "..", "test", "fixtures", "par2", "subdir.par2")
	if err := os.WriteFile(out, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Logf("wrote %s (%d bytes)", out, len(content))
}
