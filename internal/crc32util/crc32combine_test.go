package crc32util

import (
	"hash/crc32"
	"math/rand"
	"testing"
)

// crc32IEEE computes the standard CRC32 IEEE checksum of data.
func crc32IEEE(data []byte) uint32 {
	return crc32.ChecksumIEEE(data)
}

func TestCombine_TwoParts(t *testing.T) {
	data := []byte("Hello, World! This is a test of CRC32 combine.")
	mid := len(data) / 2
	part1 := data[:mid]
	part2 := data[mid:]

	crc1 := crc32IEEE(part1)
	crc2 := crc32IEEE(part2)

	combined := Combine(crc1, crc2, int64(len(part2)))
	expected := crc32IEEE(data)

	if combined != expected {
		t.Errorf("Combine(%08x, %08x, %d) = %08x, want %08x",
			crc1, crc2, len(part2), combined, expected)
	}
}

func TestCombine_ManyParts(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	expected := crc32IEEE(data)

	// Split into 12 parts of varying sizes.
	splits := []int{1, 3, 5, 2, 7, 4, 6, 3, 8, 5, 10, len(data) - 54}

	// Verify splits sum to len(data).
	sum := 0
	for _, s := range splits {
		sum += s
	}
	if sum != len(data) {
		t.Fatalf("splits sum to %d, want %d", sum, len(data))
	}

	var accumulated uint32
	offset := 0
	for i, size := range splits {
		part := data[offset : offset+size]
		partCRC := crc32IEEE(part)
		if i == 0 {
			accumulated = partCRC
		} else {
			accumulated = Combine(accumulated, partCRC, int64(size))
		}
		offset += size
	}

	if accumulated != expected {
		t.Errorf("ManyParts: accumulated = %08x, want %08x", accumulated, expected)
	}
}

func TestCombine_EmptySecond(t *testing.T) {
	data := []byte("some data here")
	crc1 := crc32IEEE(data)

	// Combining with an empty second part should return crc1.
	combined := Combine(crc1, 0, 0)
	if combined != crc1 {
		t.Errorf("Combine with empty second: got %08x, want %08x", combined, crc1)
	}

	// Also test with negative length.
	combined = Combine(crc1, 0, -1)
	if combined != crc1 {
		t.Errorf("Combine with negative len2: got %08x, want %08x", combined, crc1)
	}
}

func TestCombine_EmptyFirst(t *testing.T) {
	data := []byte("some data here")
	crc2 := crc32IEEE(data)

	// CRC of empty data is 0.
	combined := Combine(0, crc2, int64(len(data)))
	expected := crc32IEEE(data)

	if combined != expected {
		t.Errorf("Combine with empty first: got %08x, want %08x", combined, expected)
	}
}

func TestCombine_SingleByte(t *testing.T) {
	data := []byte("The quick brown fox jumps over the lazy dog")
	expected := crc32IEEE(data)

	// Accumulate one byte at a time.
	var accumulated uint32
	for i, b := range data {
		byteCRC := crc32IEEE([]byte{b})
		if i == 0 {
			accumulated = byteCRC
		} else {
			accumulated = Combine(accumulated, byteCRC, 1)
		}
	}

	if accumulated != expected {
		t.Errorf("SingleByte: accumulated = %08x, want %08x", accumulated, expected)
	}
}

func TestCombine_LargeLen(t *testing.T) {
	// 1MB of pseudo-random data.
	rng := rand.New(rand.NewSource(42))
	data := make([]byte, 1<<20)
	rng.Read(data)

	expected := crc32IEEE(data)

	// Split at various points.
	splits := []int{
		100000, 200000, 300000, 150000, 50000,
		100000, 48576, 100000,
	}

	sum := 0
	for _, s := range splits {
		sum += s
	}
	// Adjust last split to match total.
	splits[len(splits)-1] += len(data) - sum

	var accumulated uint32
	offset := 0
	for i, size := range splits {
		part := data[offset : offset+size]
		partCRC := crc32IEEE(part)
		if i == 0 {
			accumulated = partCRC
		} else {
			accumulated = Combine(accumulated, partCRC, int64(size))
		}
		offset += size
	}

	if accumulated != expected {
		t.Errorf("LargeLen: accumulated = %08x, want %08x", accumulated, expected)
	}
}

func TestCombine_KnownVectors(t *testing.T) {
	tests := []struct {
		name string
		data string
		crc  uint32
	}{
		{"empty", "", 0x00000000},
		{"a", "a", 0xE8B7BE43},
		{"abc", "abc", 0x352441C2},
		{"123456789", "123456789", 0xCBF43926},
		{"fox", "The quick brown fox jumps over the lazy dog", 0x414FA339},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := crc32IEEE([]byte(tt.data))
			if got != tt.crc {
				t.Errorf("CRC32(%q) = %08x, want %08x", tt.data, got, tt.crc)
			}
		})
	}

	// Now verify Combine works across known vectors.
	// "123456789" = "1234" + "56789"
	crc1 := crc32IEEE([]byte("1234"))
	crc2 := crc32IEEE([]byte("56789"))
	combined := Combine(crc1, crc2, 5)
	if combined != 0xCBF43926 {
		t.Errorf("Combine(CRC(1234), CRC(56789), 5) = %08x, want CBF43926", combined)
	}

	// "abc" = "a" + "bc"
	crcA := crc32IEEE([]byte("a"))
	crcBC := crc32IEEE([]byte("bc"))
	combined = Combine(crcA, crcBC, 2)
	if combined != 0x352441C2 {
		t.Errorf("Combine(CRC(a), CRC(bc), 2) = %08x, want 352441C2", combined)
	}
}

func TestCombine_Associative(t *testing.T) {
	// Verify that combining is associative:
	// Combine(Combine(A, B), C) == Combine(A, Combine(B, C))
	// Note: this isn't directly true because Combine needs the length of
	// the second argument. But sequential combination should produce the
	// same result regardless of grouping.
	a := []byte("Hello")
	b := []byte(", ")
	c := []byte("World!")

	crcA := crc32IEEE(a)
	crcB := crc32IEEE(b)
	crcC := crc32IEEE(c)

	// Left-to-right: ((A, B), C)
	ab := Combine(crcA, crcB, int64(len(b)))
	abc1 := Combine(ab, crcC, int64(len(c)))

	// Verify against direct computation.
	expected := crc32IEEE([]byte("Hello, World!"))
	if abc1 != expected {
		t.Errorf("Left-to-right: got %08x, want %08x", abc1, expected)
	}
}

func TestZeroUnpad_RoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		nZeros int
	}{
		{"short_data_1_zero", "Hello", 1},
		{"short_data_10_zeros", "Hello", 10},
		{"short_data_100_zeros", "Hello", 100},
		{"medium_data_50_zeros", "The quick brown fox jumps over the lazy dog", 50},
		{"single_byte_1_zero", "x", 1},
		{"single_byte_1000_zeros", "x", 1000},
		{"longer_data", "abcdefghijklmnopqrstuvwxyz0123456789", 256},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origData := []byte(tt.data)
			origCRC := crc32IEEE(origData)

			// Pad with zeros.
			padded := make([]byte, len(origData)+tt.nZeros)
			copy(padded, origData)
			// Remaining bytes are already zero.

			paddedCRC := crc32IEEE(padded)

			// Unpad should recover the original CRC.
			recovered := ZeroUnpad(paddedCRC, int64(tt.nZeros))

			if recovered != origCRC {
				t.Errorf("ZeroUnpad(%08x, %d) = %08x, want %08x (original CRC of %q)",
					paddedCRC, tt.nZeros, recovered, origCRC, tt.data)
			}
		})
	}
}

func TestZeroUnpad_ZeroLength(t *testing.T) {
	crc := crc32IEEE([]byte("test"))
	if ZeroUnpad(crc, 0) != crc {
		t.Error("ZeroUnpad with n=0 should return input unchanged")
	}
	if ZeroUnpad(crc, -1) != crc {
		t.Error("ZeroUnpad with n<0 should return input unchanged")
	}
}

func TestZeroUnpad_LargeN(t *testing.T) {
	data := []byte("test data for large zero unpad")
	origCRC := crc32IEEE(data)

	nZeros := 10000
	padded := make([]byte, len(data)+nZeros)
	copy(padded, data)

	paddedCRC := crc32IEEE(padded)
	recovered := ZeroUnpad(paddedCRC, int64(nZeros))

	if recovered != origCRC {
		t.Errorf("ZeroUnpad large: got %08x, want %08x", recovered, origCRC)
	}
}

func TestZeroUnpad_CombineConsistency(t *testing.T) {
	// Verify that ZeroUnpad is consistent with Combine:
	// CRC(data || zeros) = Combine(CRC(data), CRC(zeros), nZeros)
	// So ZeroUnpad(Combine(crc_data, crc_zeros, n), n) should give crc_data.
	data := []byte("consistency check data")
	nZeros := 500
	zeros := make([]byte, nZeros)

	crcData := crc32IEEE(data)
	crcZeros := crc32IEEE(zeros)

	combined := Combine(crcData, crcZeros, int64(nZeros))

	// Verify combined equals CRC of data||zeros.
	full := make([]byte, len(data)+nZeros)
	copy(full, data)
	expectedCombined := crc32IEEE(full)
	if combined != expectedCombined {
		t.Fatalf("Combine mismatch: got %08x, want %08x", combined, expectedCombined)
	}

	// Now unpad.
	recovered := ZeroUnpad(combined, int64(nZeros))
	if recovered != crcData {
		t.Errorf("ZeroUnpad after Combine: got %08x, want %08x", recovered, crcData)
	}
}

// BenchmarkCombine benchmarks the Combine function with various lengths.
func BenchmarkCombine(b *testing.B) {
	crc1 := crc32IEEE([]byte("benchmark data part 1"))
	crc2 := crc32IEEE([]byte("benchmark data part 2"))

	for _, len2 := range []int64{1, 100, 10000, 1000000} {
		b.Run("len="+string(rune('0'+len2)), func(b *testing.B) {
			for b.Loop() {
				Combine(crc1, crc2, len2)
			}
		})
	}
}

func TestMatrixMath(t *testing.T) {
	// Identity matrix
	var identity [32]uint32
	for i := range 32 {
		identity[i] = 1 << i
	}

	// 1. gf2MatrixMul with identity matrix
	var vec uint32 = 0xDEADC0DE
	if got := gf2MatrixMul(&identity, vec); got != vec {
		t.Errorf("gf2MatrixMul(identity, %x) = %x, want %x", vec, got, vec)
	}

	// 2. gf2MatrixMul with zero vector
	if got := gf2MatrixMul(&identity, 0); got != 0 {
		t.Errorf("gf2MatrixMul(identity, 0) = %x, want 0", got)
	}

	// 3. gf2MatrixSquare of identity
	var squared [32]uint32
	gf2MatrixSquare(&squared, &identity)
	for i := range 32 {
		if squared[i] != identity[i] {
			t.Errorf("gf2MatrixSquare(identity)[%d] = %x, want %x", i, squared[i], identity[i])
		}
	}

	// 4. gf2MatrixInvert of identity
	var inverted [32]uint32
	gf2MatrixInvert(&inverted, &identity)
	for i := range 32 {
		if inverted[i] != identity[i] {
			t.Errorf("gf2MatrixInvert(identity)[%d] = %x, want %x", i, inverted[i], identity[i])
		}
	}

	// 5. Singular matrix inversion (all zeroes)
	var singular [32]uint32
	var invSingular [32]uint32
	gf2MatrixInvert(&invSingular, &singular)
	for i := range 32 {
		if invSingular[i] != 0 {
			t.Errorf("gf2MatrixInvert(singular)[%d] = %x, want 0", i, invSingular[i])
		}
	}
}

func TestShiftHelpers(t *testing.T) {
	// 1. oneByteShift output verification (sanity check)
	mat := oneByteShift()
	nonZero := false
	for _, row := range mat {
		if row != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Error("oneByteShift returned an all-zero matrix")
	}

	// 2. crcOfZeros boundary cases
	if got := crcOfZeros(0); got != 0 {
		t.Errorf("crcOfZeros(0) = %x, want 0", got)
	}
	if got := crcOfZeros(-5); got != 0 {
		t.Errorf("crcOfZeros(-5) = %x, want 0", got)
	}

	// 3. applyShift boundary cases
	val := uint32(12345)
	if got := applyShift(val, 0); got != val {
		t.Errorf("applyShift(val, 0) = %x, want %x", got, val)
	}
	if got := applyShift(val, -1); got != val {
		t.Errorf("applyShift(val, -1) = %x, want %x", got, val)
	}

	// 4. applyInverseShift boundary cases
	if got := applyInverseShift(val, 0); got != val {
		t.Errorf("applyInverseShift(val, 0) = %x, want %x", got, val)
	}
	if got := applyInverseShift(val, -1); got != val {
		t.Errorf("applyInverseShift(val, -1) = %x, want %x", got, val)
	}
}
