package dirscanner

import (
	"archive/zip"
	"compress/gzip"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------- DetectType ----------

func TestDetectType_AllTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want ArchiveType
	}{
		{"file.nzb", TypeNZB},
		{"file.NZB", TypeNZB},
		{"file.nzb.gz", TypeGZ},
		{"file.NZB.GZ", TypeGZ},
		{"file.nzb.bz2", TypeBZ2},
		{"file.NZB.BZ2", TypeBZ2},
		{"file.zip", TypeZip},
		{"file.ZIP", TypeZip},
	}
	for _, tt := range tests {
		got, err := DetectType(tt.path)
		if err != nil {
			t.Errorf("DetectType(%q) error: %v", tt.path, err)
			continue
		}
		if got != tt.want {
			t.Errorf("DetectType(%q) = %d, want %d", tt.path, got, tt.want)
		}
	}
}

func TestDetectType_Unknown(t *testing.T) {
	t.Parallel()
	_, err := DetectType("file.rar")
	if err == nil {
		t.Error("DetectType(.rar) should return error")
	}
}

// ---------- ExtractNZBs ----------

func TestExtractNZBs_PlainNZB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.nzb")
	content := []byte("<nzb>test</nzb>")
	os.WriteFile(path, content, 0644)

	results, err := ExtractNZBs(path)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if string(results[0]) != string(content) {
		t.Errorf("content mismatch")
	}
}

func TestExtractNZBs_GZip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.nzb.gz")
	content := []byte("<nzb>gzipped</nzb>")

	f, _ := os.Create(path)
	w := gzip.NewWriter(f)
	w.Write(content)
	w.Close()
	f.Close()

	results, err := ExtractNZBs(path)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if string(results[0]) != string(content) {
		t.Errorf("content mismatch: got %q", results[0])
	}
}

func TestExtractNZBs_Zip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.zip")
	content := []byte("<nzb>zipped</nzb>")

	f, _ := os.Create(path)
	w := zip.NewWriter(f)
	fw, _ := w.Create("inner.nzb")
	fw.Write(content)
	w.Close()
	f.Close()

	results, err := ExtractNZBs(path)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if string(results[0]) != string(content) {
		t.Errorf("content mismatch")
	}
}

func TestExtractNZBs_ZipNoNZBs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.zip")

	f, _ := os.Create(path)
	w := zip.NewWriter(f)
	fw, _ := w.Create("readme.txt")
	fw.Write([]byte("no nzb here"))
	w.Close()
	f.Close()

	_, err := ExtractNZBs(path)
	if err == nil {
		t.Error("expected error for zip with no .nzb files")
	}
}

func TestExtractNZBs_ZipMultipleNZBs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.zip")

	f, _ := os.Create(path)
	w := zip.NewWriter(f)
	for _, name := range []string{"a.nzb", "b.nzb", "readme.txt"} {
		fw, _ := w.Create(name)
		fw.Write([]byte("<nzb>" + name + "</nzb>"))
	}
	w.Close()
	f.Close()

	results, err := ExtractNZBs(path)
	if err != nil {
		t.Fatalf("ExtractNZBs: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("got %d NZBs, want 2 (skipping readme.txt)", len(results))
	}
}

func TestExtractNZBs_NonexistentFile(t *testing.T) {
	_, err := ExtractNZBs("/nonexistent/file.nzb")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestExtractNZBs_InvalidGzip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.nzb.gz")
	os.WriteFile(path, []byte("not gzip data"), 0644)

	_, err := ExtractNZBs(path)
	if err == nil {
		t.Error("expected error for invalid gzip")
	}
}

func TestExtractNZBs_UnknownType(t *testing.T) {
	_, err := ExtractNZBs("/some/file.rar")
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

// ---------- Store (state persistence) ----------

func TestStore_OpenNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if store == nil {
		t.Fatal("store is nil")
	}
}

func TestStore_GetSetDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, _ := OpenStore(path)

	now := time.Now().Truncate(time.Second)
	store.Set("file1.nzb", FileState{Size: 1234, MTime: now})

	got, ok := store.Get("file1.nzb")
	if !ok {
		t.Fatal("Get: not found after Set")
	}
	if got.Size != 1234 {
		t.Errorf("Size = %d, want 1234", got.Size)
	}

	store.Delete("file1.nzb")
	_, ok = store.Get("file1.nzb")
	if ok {
		t.Error("still found after Delete")
	}
}

func TestStore_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, _ := OpenStore(path)

	// Should not panic.
	store.Delete("nonexistent")
}

func TestStore_SaveAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, _ := OpenStore(path)

	now := time.Now().Truncate(time.Second)
	store.Set("file1.nzb", FileState{Size: 5678, MTime: now})

	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload.
	store2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore reload: %v", err)
	}
	got, ok := store2.Get("file1.nzb")
	if !ok {
		t.Fatal("not found after reload")
	}
	if got.Size != 5678 {
		t.Errorf("Size = %d, want 5678", got.Size)
	}
}

func TestStore_SaveNotDirtyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, _ := OpenStore(path)

	// No changes: save should be a no-op.
	if err := store.Save(); err != nil {
		t.Fatalf("Save (not dirty): %v", err)
	}

	// File should NOT exist since nothing was written.
	if _, err := os.Stat(path); err == nil {
		t.Error("file created even though store was not dirty")
	}
}

func TestStore_OpenCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("not json {{{"), 0644)

	_, err := OpenStore(path)
	if err == nil {
		t.Error("expected error for corrupt JSON")
	}
}

// ---------- Direct Decompress Helpers ----------

func TestExtractPlainNZB_Direct(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.nzb")
	content := []byte("<nzb>plain</nzb>")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := extractPlainNZB(path)
	if err != nil {
		t.Fatalf("extractPlainNZB failed: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}

	// Exceed max size
	largePath := filepath.Join(tmp, "large.nzb")
	largeData := make([]byte, MaxDecompressSize+100)
	if err := os.WriteFile(largePath, largeData, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractPlainNZB(largePath); err == nil {
		t.Error("expected error for file exceeding MaxDecompressSize")
	}
}

func TestExtractGZ_Direct(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.nzb.gz")
	content := []byte("<nzb>gzip</nzb>")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(f)
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()

	got, err := extractGZ(path)
	if err != nil {
		t.Fatalf("extractGZ failed: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}
}

func TestExtractBZ2_Direct(t *testing.T) {
	bz2Hex := "425a683931415926535967ae97bf0000011980000080051621401020003100301a1a0c9366c315acf714c2f17724538509067ae97bf0"
	bz2Bytes, err := hex.DecodeString(bz2Hex)
	if err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.nzb.bz2")
	if err := os.WriteFile(path, bz2Bytes, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := extractBZ2(path)
	if err != nil {
		t.Fatalf("extractBZ2 failed: %v", err)
	}
	expected := "<nzb>bzipped</nzb>"
	if len(got) != 1 || string(got[0]) != expected {
		t.Errorf("got %q, want %q", got, expected)
	}
}

func TestExtractZip_Direct(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "test.zip")
	content := []byte("<nzb>zipped</nzb>")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	fw, err := w.Create("inner.nzb")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	w.Close()
	f.Close()

	got, err := extractZip(path)
	if err != nil {
		t.Fatalf("extractZip failed: %v", err)
	}
	if len(got) != 1 || string(got[0]) != string(content) {
		t.Errorf("got %q, want %q", got, content)
	}
}

// bz2OfZeros maps a zero-byte payload length to its pre-computed bzip2
// encoding (Go's standard library only provides a bzip2 reader, not a
// writer). Generated via: python3 -c "import bz2; print(bz2.compress(bytes(N)).hex())"
var bz2OfZeros = map[int]string{
	10: "425a68393141592653596e1651c7000000400041002000210082831772453850906e1651c7",
	11: "425a6839314159265359378de1d600000040004080200021008283177245385090378de1d6",
}

func compressBZ2(t *testing.T, data []byte) string {
	t.Helper()
	for _, b := range data {
		if b != 0 {
			t.Fatalf("compressBZ2 only supports all-zero payloads")
		}
	}
	hexData, ok := bz2OfZeros[len(data)]
	if !ok {
		t.Fatalf("compressBZ2: no precomputed fixture for length %d", len(data))
	}
	raw, err := hex.DecodeString(hexData)
	if err != nil {
		t.Fatalf("decode bz2 fixture: %v", err)
	}
	tmp := t.TempDir()
	outPath := filepath.Join(tmp, "temp.bz2")
	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		t.Fatalf("write bz2 fixture: %v", err)
	}
	return outPath
}

func TestMaxDecompressSizeBoundary(t *testing.T) {
	// Temporarily modify MaxDecompressSize
	oldLimit := MaxDecompressSize
	MaxDecompressSize = 10
	defer func() { MaxDecompressSize = oldLimit }()

	tmp := t.TempDir()

	// 1. extractPlainNZB
	// Exactly 10 bytes (should succeed)
	p1 := filepath.Join(tmp, "p1.nzb")
	os.WriteFile(p1, make([]byte, 10), 0644)
	res, err := extractPlainNZB(p1)
	if err != nil {
		t.Errorf("extractPlainNZB exactly at boundary failed: %v", err)
	}
	if len(res) != 1 || len(res[0]) != 10 {
		t.Errorf("expected 1 result of length 10")
	}

	// 11 bytes (should fail)
	p2 := filepath.Join(tmp, "p2.nzb")
	os.WriteFile(p2, make([]byte, 11), 0644)
	_, err = extractPlainNZB(p2)
	if err == nil {
		t.Errorf("expected error for extractPlainNZB exceeding boundary")
	}

	// 2. extractGZ
	// Exactly 10 bytes (should succeed)
	gz1 := filepath.Join(tmp, "gz1.nzb.gz")
	{
		f, _ := os.Create(gz1)
		w := gzip.NewWriter(f)
		w.Write(make([]byte, 10))
		w.Close()
		f.Close()
	}
	res, err = extractGZ(gz1)
	if err != nil {
		t.Errorf("extractGZ exactly at boundary failed: %v", err)
	}
	if len(res) != 1 || len(res[0]) != 10 {
		t.Errorf("expected 1 result of length 10")
	}

	// 11 bytes (should fail)
	gz2 := filepath.Join(tmp, "gz2.nzb.gz")
	{
		f, _ := os.Create(gz2)
		w := gzip.NewWriter(f)
		w.Write(make([]byte, 11))
		w.Close()
		f.Close()
	}
	_, err = extractGZ(gz2)
	if err == nil {
		t.Errorf("expected error for extractGZ exceeding boundary")
	}

	// 3. extractZip
	// Exactly 10 bytes (should succeed)
	z1 := filepath.Join(tmp, "z1.zip")
	{
		f, _ := os.Create(z1)
		w := zip.NewWriter(f)
		fw, _ := w.Create("inner.nzb")
		fw.Write(make([]byte, 10))
		w.Close()
		f.Close()
	}
	res, err = extractZip(z1)
	if err != nil {
		t.Errorf("extractZip exactly at boundary failed: %v", err)
	}
	if len(res) != 1 || len(res[0]) != 10 {
		t.Errorf("expected 1 result of length 10")
	}

	// 11 bytes (should fail)
	z2 := filepath.Join(tmp, "z2.zip")
	{
		f, _ := os.Create(z2)
		w := zip.NewWriter(f)
		fw, _ := w.Create("inner.nzb")
		fw.Write(make([]byte, 11))
		w.Close()
		f.Close()
	}
	_, err = extractZip(z2)
	if err == nil {
		t.Errorf("expected error for extractZip exceeding boundary")
	}

	// 4. extractBZ2
	// Exactly 10 bytes (should succeed)
	bz1 := compressBZ2(t, make([]byte, 10))
	res, err = extractBZ2(bz1)
	if err != nil {
		t.Errorf("extractBZ2 exactly at boundary failed: %v", err)
	}
	if len(res) != 1 || len(res[0]) != 10 {
		t.Errorf("expected 1 result of length 10")
	}

	// 11 bytes (should fail)
	bz2 := compressBZ2(t, make([]byte, 11))
	_, err = extractBZ2(bz2)
	if err == nil {
		t.Errorf("expected error for extractBZ2 exceeding boundary")
	}
}

func TestZipCustomLimits(t *testing.T) {
	oldMaxNZBs := maxNZBsPerZip
	oldMaxCumulative := maxCumulativeBytes
	defer func() {
		maxNZBsPerZip = oldMaxNZBs
		maxCumulativeBytes = oldMaxCumulative
	}()

	maxNZBsPerZip = 2
	maxCumulativeBytes = 20

	tmp := t.TempDir()

	// Test maxNZBsPerZip limit (3 files in zip, max is 2)
	z1 := filepath.Join(tmp, "nzb_limit.zip")
	{
		f, _ := os.Create(z1)
		w := zip.NewWriter(f)
		for _, name := range []string{"1.nzb", "2.nzb", "3.nzb"} {
			fw, _ := w.Create(name)
			fw.Write([]byte("x")) // 1 byte each
		}
		w.Close()
		f.Close()
	}
	_, err := extractZip(z1)
	if err == nil {
		t.Errorf("expected error for zip exceeding maxNZBsPerZip")
	} else if !strings.Contains(err.Error(), "more than 2 NZB files") {
		t.Errorf("expected max NZB files error, got: %v", err)
	}

	// Test maxCumulativeBytes limit (2 files of 11 bytes, total 22, max is 20)
	z2 := filepath.Join(tmp, "bytes_limit.zip")
	{
		f, _ := os.Create(z2)
		w := zip.NewWriter(f)
		for _, name := range []string{"1.nzb", "2.nzb"} {
			fw, _ := w.Create(name)
			fw.Write(make([]byte, 11))
		}
		w.Close()
		f.Close()
	}
	_, err = extractZip(z2)
	if err == nil {
		t.Errorf("expected error for zip exceeding maxCumulativeBytes")
	} else if !strings.Contains(err.Error(), "cumulative decompressed size exceeds") {
		t.Errorf("expected cumulative size error, got: %v", err)
	}

	// Test cumulative boundary: exactly equal to maxCumulativeBytes (20) succeeds
	z3 := filepath.Join(tmp, "bytes_boundary.zip")
	{
		f, _ := os.Create(z3)
		w := zip.NewWriter(f)
		for _, name := range []string{"1.nzb", "2.nzb"} {
			fw, _ := w.Create(name)
			fw.Write(make([]byte, 10)) // 2 * 10 = 20 bytes
		}
		w.Close()
		f.Close()
	}
	res, err := extractZip(z3)
	if err != nil {
		t.Errorf("extractZip exactly at cumulative limit failed: %v", err)
	}
	if len(res) != 2 {
		t.Errorf("expected 2 results, got %d", len(res))
	}
}
