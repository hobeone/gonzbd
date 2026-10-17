package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hobeone/gonzbd/internal/nzb"
)

func TestParseSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "1024", want: 1024},
		{in: "64K", want: 64 << 10},
		{in: "64KB", want: 64 << 10},
		{in: "128M", want: 128 << 20},
		{in: "128MB", want: 128 << 20},
		{in: "2G", want: 2 << 30},
		{in: "2GB", want: 2 << 30},
		{in: "", wantErr: true},
		{in: "-5M", wantErr: true},
		{in: "0", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "10T", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseSize(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestWriteDeterministicPayload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p1 := filepath.Join(dir, "p1.bin")
	p2 := filepath.Join(dir, "p2.bin")
	p3 := filepath.Join(dir, "p3.bin")

	const size = 100_000
	sum1, err := WriteDeterministicPayload(p1, size, 42)
	if err != nil {
		t.Fatalf("WriteDeterministicPayload p1: %v", err)
	}
	sum2, err := WriteDeterministicPayload(p2, size, 42)
	if err != nil {
		t.Fatalf("WriteDeterministicPayload p2: %v", err)
	}
	sum3, err := WriteDeterministicPayload(p3, size, 99)
	if err != nil {
		t.Fatalf("WriteDeterministicPayload p3: %v", err)
	}

	if sum1 != sum2 {
		t.Fatalf("same seed produced different SHA-256: %s vs %s", sum1, sum2)
	}
	if sum1 == sum3 {
		t.Fatalf("different seeds produced identical SHA-256: %s", sum1)
	}

	raw, err := os.ReadFile(p1)
	if err != nil {
		t.Fatalf("ReadFile p1: %v", err)
	}
	if int64(len(raw)) != size {
		t.Fatalf("len(raw) = %d, want %d", len(raw), size)
	}
	wantSum := sha256.Sum256(raw)
	if gotHex := hex.EncodeToString(wantSum[:]); gotHex != sum1 {
		t.Fatalf("returned SHA-256 %s != actual file SHA-256 %s", sum1, gotHex)
	}
}

func TestSliceFileSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fpath := filepath.Join(dir, "data.bin")
	payload := bytes.Repeat([]byte("0123456789abcdef"), 100) // 1600 bytes
	if err := os.WriteFile(fpath, payload, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rf, err := SliceFileSegments("relA/1", fpath, 0, "data.bin", "obf_data.bin", RoleRARVolume, 1, 500)
	if err != nil {
		t.Fatalf("SliceFileSegments: %v", err)
	}
	other, err := SliceFileSegments("relB/1", fpath, 0, "data.bin", "obf_data.bin", RoleRARVolume, 1, 500)
	if err != nil {
		t.Fatalf("SliceFileSegments other namespace: %v", err)
	}
	for i := range rf.Segments {
		if rf.Segments[i].MessageID == other.Segments[i].MessageID {
			t.Fatalf("seg[%d] Message-ID %q is shared by two namespaces; a multi-release server would refuse them", i, rf.Segments[i].MessageID)
		}
	}
	if rf.TotalSize != 1600 {
		t.Fatalf("TotalSize = %d, want 1600", rf.TotalSize)
	}
	if len(rf.Segments) != 4 {
		t.Fatalf("len(Segments) = %d, want 4", len(rf.Segments))
	}
	wantLengths := []int{500, 500, 500, 100}
	var offset int64
	for i, seg := range rf.Segments {
		if seg.PartNum != i+1 {
			t.Fatalf("seg[%d].PartNum = %d, want %d", i, seg.PartNum, i+1)
		}
		if seg.Offset != offset {
			t.Fatalf("seg[%d].Offset = %d, want %d", i, seg.Offset, offset)
		}
		if seg.Length != wantLengths[i] {
			t.Fatalf("seg[%d].Length = %d, want %d", i, seg.Length, wantLengths[i])
		}
		wantCRC := crc32.ChecksumIEEE(payload[offset : offset+int64(seg.Length)])
		if seg.CRC32 != wantCRC {
			t.Fatalf("seg[%d].CRC32 = %08x, want %08x", i, seg.CRC32, wantCRC)
		}
		offset += int64(seg.Length)
	}
}

func TestManifestAndNZBRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := &ReleaseManifest{
		Name:     "test-release",
		PartSize: 500,
		ExpectedOutputs: []ExpectedOutput{
			{Name: "payload.bin", Size: 1600, SHA256: "deadbeef"},
		},
		Files: []ReleaseFile{
			{
				FileIndex:   0,
				DiskName:    "release.part01.rar",
				NZBFilename: "release.part01.rar",
				Role:        RoleRARVolume,
				VolumeNum:   1,
				TotalSize:   1000,
				SHA256:      "cafebabe",
				Segments: []SegmentMeta{
					{MessageID: "seg1@localnntp.test", PartNum: 1, Offset: 0, Length: 500, CRC32: 0x11223344},
					{MessageID: "seg2@localnntp.test", PartNum: 2, Offset: 500, Length: 500, CRC32: 0x55667788},
				},
			},
		},
	}

	mpath := filepath.Join(dir, "manifest.json")
	if err := SaveManifest(mpath, m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	loaded, err := LoadManifest(mpath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if loaded.Name != m.Name || len(loaded.Files) != 1 || len(loaded.Files[0].Segments) != 2 {
		t.Fatalf("loaded manifest mismatch: %+v", loaded)
	}

	if _, err := LoadManifest(filepath.Join(dir, "nonexistent.json")); err == nil {
		t.Fatal("LoadManifest on nonexistent path succeeded, want error")
	}

	nzbBytes := BuildNZB(m)
	parsed, err := nzb.Parse(bytes.NewReader(nzbBytes))
	if err != nil {
		t.Fatalf("nzb.Parse: %v", err)
	}
	if len(parsed.Files) != 1 {
		t.Fatalf("len(parsed.Files) = %d, want 1", len(parsed.Files))
	}
	if len(parsed.Files[0].Articles) != 2 {
		t.Fatalf("len(Articles) = %d, want 2", len(parsed.Files[0].Articles))
	}
	if parsed.Files[0].Articles[0].ID != "seg1@localnntp.test" {
		t.Fatalf("Articles[0].ID = %q, want seg1@localnntp.test", parsed.Files[0].Articles[0].ID)
	}
}

func TestClassifyReleaseFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		wantRole FileRole
		wantVol  int
	}{
		{name: "release.part01.rar", wantRole: RoleRARVolume, wantVol: 1},
		{name: "release.part12.rar", wantRole: RoleRARVolume, wantVol: 12},
		{name: "release.rar", wantRole: RoleRARVolume, wantVol: 1},
		{name: "release.par2", wantRole: RolePar2Index, wantVol: 0},
		{name: "release.vol00+01.par2", wantRole: RolePar2Volume, wantVol: 1},
		{name: "release.vol01+02.par2", wantRole: RolePar2Volume, wantVol: 2},
		{name: "release.nfo", wantRole: RoleSidecar, wantVol: 0},
		{name: "sample.mkv", wantRole: RoleSidecar, wantVol: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			role, vol := classifyReleaseFile(tc.name)
			if role != tc.wantRole || vol != tc.wantVol {
				t.Fatalf("classifyReleaseFile(%q) = (%q, %d), want (%q, %d)",
					tc.name, role, vol, tc.wantRole, tc.wantVol)
			}
		})
	}
}

func TestGenerateRelease_EndToEnd(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("rar"); err != nil {
		t.Skip("rar not installed")
	}
	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := filepath.Join(t.TempDir(), "rel")
	cfg := GenConfig{
		OutDir:         dir,
		Name:           "synth-movie",
		PayloadSize:    512 << 10, // 512 KiB
		Volumes:        4,
		PartSize:       64 << 10, // 64 KiB articles
		Par2Redundancy: 10,
		Par2Volumes:    2,
		Seed:           12345,
		Obfuscate:      true,
		IncludeSidecar: true,
	}

	manifest, err := GenerateRelease(cfg)
	if err != nil {
		t.Fatalf("GenerateRelease: %v", err)
	}
	if _, err := GenerateRelease(cfg); err == nil || !strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("GenerateRelease on populated dir = %v, want non-empty error", err)
	}
	if len(manifest.ExpectedOutputs) != 1 || manifest.ExpectedOutputs[0].Name != "payload.bin" {
		t.Fatalf("ExpectedOutputs = %+v, want [payload.bin]", manifest.ExpectedOutputs)
	}

	var rarCount, par2IdxCount, par2VolCount, sidecarCount int
	for _, f := range manifest.Files {
		switch f.Role {
		case RoleRARVolume:
			rarCount++
			if f.NZBFilename == f.DiskName {
				t.Errorf("expected obfuscated NZBFilename for RAR volume %s, got %s", f.DiskName, f.NZBFilename)
			}
		case RolePar2Index:
			par2IdxCount++
		case RolePar2Volume:
			par2VolCount++
		case RoleSidecar:
			sidecarCount++
		}
		if len(f.Segments) == 0 {
			t.Errorf("file %s has 0 segments", f.DiskName)
		}
	}
	if rarCount < 2 {
		t.Errorf("rarCount = %d, want >= 2", rarCount)
	}
	if par2IdxCount != 1 {
		t.Errorf("par2IdxCount = %d, want 1", par2IdxCount)
	}
	if par2VolCount < 1 {
		t.Errorf("par2VolCount = %d, want >= 1", par2VolCount)
	}
	if sidecarCount != 2 {
		t.Errorf("sidecarCount = %d, want 2 (nfo + sample.mkv)", sidecarCount)
	}

	// Verify manifest.json, release.nzb, and articles.pack were written to OutDir.
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Errorf("manifest.json missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "articles.pack")); err != nil {
		t.Errorf("articles.pack missing: %v", err)
	}
	for _, f := range manifest.Files {
		for _, seg := range f.Segments {
			if seg.WireLength <= 0 {
				t.Errorf("segment %s has invalid WireLength: %d", seg.MessageID, seg.WireLength)
			}
		}
	}
	nzbRaw, err := os.ReadFile(filepath.Join(dir, "synth-movie.nzb"))
	if err != nil {
		t.Fatalf("synth-movie.nzb missing: %v", err)
	}
	if !strings.Contains(string(nzbRaw), "<nzb") {
		t.Errorf("synth-movie.nzb does not contain <nzb")
	}

	// Verify validation errors on bad GenConfig.
	if _, err := GenerateRelease(GenConfig{OutDir: t.TempDir(), PayloadSize: 0, Volumes: 1, PartSize: 1024}); err == nil {
		t.Error("expected error for PayloadSize=0")
	}
	if _, err := GenerateRelease(GenConfig{OutDir: t.TempDir(), PayloadSize: 1024, Volumes: 0, PartSize: 1024}); err == nil {
		t.Error("expected error for Volumes=0")
	}
	if _, err := GenerateRelease(GenConfig{OutDir: t.TempDir(), PayloadSize: 1024, Volumes: 1, PartSize: 0}); err == nil {
		t.Error("expected error for PartSize=0")
	}
}

// Releases that differ in name or in seed must not share a Message-ID, or a multi-release
// server refuses to index them together. sample.mkv is generated for every release, so a
// namespace that ignored name or seed would collide on it.
func TestGenerateRelease_MessageIDNamespace(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("rar"); err != nil {
		t.Skip("rar not installed")
	}

	gen := func(name string, seed uint64) map[string]bool {
		m, err := GenerateRelease(GenConfig{
			OutDir:         filepath.Join(t.TempDir(), "rel"),
			Name:           name,
			PayloadSize:    64 << 10,
			Volumes:        2,
			PartSize:       16 << 10,
			Seed:           seed,
			IncludeSidecar: true,
		})
		if err != nil {
			t.Fatalf("GenerateRelease(%s, %d): %v", name, seed, err)
		}
		ids := map[string]bool{}
		for _, f := range m.Files {
			for _, seg := range f.Segments {
				ids[seg.MessageID] = true
			}
		}
		if len(ids) == 0 {
			t.Fatalf("GenerateRelease(%s, %d) produced no Message-IDs", name, seed)
		}
		return ids
	}

	base := gen("relA", 1)
	for _, other := range []struct {
		desc string
		ids  map[string]bool
	}{
		{"different name, same seed", gen("relB", 1)},
		{"same name, different seed", gen("relA", 2)},
	} {
		for id := range other.ids {
			if base[id] {
				t.Fatalf("%s: Message-ID %q appears in both releases", other.desc, id)
			}
		}
	}

	if got := releaseIDNamespace("relA", 1); got != "relA/1" {
		t.Fatalf("releaseIDNamespace = %q, want relA/1", got)
	}
}

func TestWriteDeterministicPayload_Errors(t *testing.T) {
	t.Parallel()
	if _, err := WriteDeterministicPayload(filepath.Join(t.TempDir(), "f.bin"), -1, 42); err == nil {
		t.Error("expected error for negative size")
	}
	if _, err := WriteDeterministicPayload("/nonexistent_dir/impossible/f.bin", 100, 42); err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

func TestSliceFileSegments_Errors(t *testing.T) {
	t.Parallel()
	fpath := filepath.Join(t.TempDir(), "dummy.bin")
	if err := os.WriteFile(fpath, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := SliceFileSegments("ns", fpath, 0, "dummy.bin", "dummy.bin", RoleSidecar, 0, 0); err == nil {
		t.Error("expected error for partSize=0")
	}
	if _, err := SliceFileSegments("ns", "/nonexistent_dir/dummy.bin", 0, "dummy.bin", "dummy.bin", RoleSidecar, 0, 100); err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestSaveAndLoadManifest_Errors(t *testing.T) {
	t.Parallel()
	m := &ReleaseManifest{Name: "test"}
	if err := SaveManifest("/nonexistent_dir/impossible/manifest.json", m); err == nil {
		t.Error("expected error saving to nonexistent directory")
	}

	badJSONPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badJSONPath, []byte("{invalid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadManifest(badJSONPath); err == nil {
		t.Error("expected error loading invalid json")
	}
}

func TestBuildNZB_Escaping(t *testing.T) {
	t.Parallel()
	m := &ReleaseManifest{
		Name: "special-chars",
		Files: []ReleaseFile{
			{
				NZBFilename: `file & with "quotes" <brackets> 'apostrophes'.rar`,
				TotalSize:   100,
				Segments: []SegmentMeta{
					{
						MessageID: `msg&<id>1@localnntp.test`,
						PartNum:   1,
						Length:    100,
					},
				},
			},
		},
	}
	nzbBytes := BuildNZB(m)
	s := string(nzbBytes)
	if !strings.Contains(s, "&amp;") || !strings.Contains(s, "&quot;") || !strings.Contains(s, "&lt;") || !strings.Contains(s, "&gt;") || !strings.Contains(s, "&apos;") {
		t.Errorf("BuildNZB did not properly XML-escape attributes/text: %s", s)
	}
}

func TestGenerateRelease_DefaultsAndCompression(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("rar"); err != nil {
		t.Skip("rar not installed")
	}
	if _, err := exec.LookPath("par2"); err != nil {
		t.Skip("par2 not installed")
	}

	dir := filepath.Join(t.TempDir(), "rel_compressed")
	cfg := GenConfig{
		OutDir:         dir,
		PayloadSize:    256 << 10,
		Volumes:        2,
		PartSize:       64 << 10,
		Par2Redundancy: 10,
		Compress:       true,
		Seed:           9999,
		IncludeSidecar: false,
	}

	manifest, err := GenerateRelease(cfg)
	if err != nil {
		t.Fatalf("GenerateRelease compressed: %v", err)
	}
	if manifest.Name != "release" {
		t.Errorf("manifest.Name = %q, want default 'release'", manifest.Name)
	}
	if manifest.Volumes < 1 {
		t.Errorf("manifest.Volumes = %d, want >= 1", manifest.Volumes)
	}

	// Test Par2Redundancy == 0 produces no .par2 files
	dirNoPar2 := filepath.Join(t.TempDir(), "rel_nopar2")
	manifestNoPar2, err := GenerateRelease(GenConfig{
		OutDir:         dirNoPar2,
		PayloadSize:    128 << 10,
		Volumes:        2,
		PartSize:       64 << 10,
		Par2Redundancy: 0,
		Seed:           1111,
		IncludeSidecar: false,
	})
	if err != nil {
		t.Fatalf("GenerateRelease Par2Redundancy=0: %v", err)
	}
	for _, f := range manifestNoPar2.Files {
		if f.Role == RolePar2Index || f.Role == RolePar2Volume {
			t.Errorf("unexpected par2 file %s when Par2Redundancy=0", f.DiskName)
		}
	}

	// Test negative Par2Redundancy returns error
	if _, err := GenerateRelease(GenConfig{OutDir: t.TempDir(), PayloadSize: 1024, Volumes: 1, PartSize: 1024, Par2Redundancy: -5}); err == nil {
		t.Error("expected error for negative Par2Redundancy")
	}

	// Test invalid OutDir (e.g. /dev/null/impossible)
	if _, err := GenerateRelease(GenConfig{OutDir: "/dev/null/impossible", PayloadSize: 100, Volumes: 1, PartSize: 100}); err == nil {
		t.Error("expected error for impossible OutDir")
	}
}
