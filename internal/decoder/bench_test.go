package decoder

import (
	"hash/crc32"
	"testing"
)

// bestCaseRaw returns a payload where no byte requires yEnc escaping.
// A byte b requires escaping when (b+42) % 256 ∈ {0, 10, 13, 61}.
// That corresponds to raw bytes {214, 224, 227, 19}. We avoid those.
func bestCaseRaw(size int) []byte {
	unsafe := map[byte]bool{214: true, 224: true, 227: true, 19: true}
	b := make([]byte, size)
	v := byte(0)
	for i := range b {
		for unsafe[v] {
			v++
		}
		b[i] = v
		v++
		if v == 0 {
			v = 1 // skip 0 since it maps to 214 which is unsafe
		}
	}
	return b
}

// worstCaseRaw returns a payload of entirely random-appearing bytes that
// exercises the escape path frequently. We use all 256 byte values
// cyclically, which gives ~1/64 escape density (4 out of 256 values escape).
func worstCaseRaw(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

const benchSize = 5 * 1024 * 1024 // 5 MB — realistic NNTP article payload

var (
	benchBestCase  []byte
	benchWorstCase []byte
)

func init() {
	bestRaw := bestCaseRaw(benchSize)
	benchBestCase = yencEncode("bench_best.bin", bestRaw)

	worstRaw := worstCaseRaw(benchSize)
	benchWorstCase = yencEncode("bench_worst.bin", worstRaw)
}

// BenchmarkDecodeArticle_BestCase measures throughput when almost no bytes
// require escape processing. This exercises the bytes.IndexByte fast path.
func BenchmarkDecodeArticle_BestCase(b *testing.B) {
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, _, _, _, err := DecodeArticle(benchBestCase)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 {
			b.Fatal("empty result")
		}
	}
}

// BenchmarkDecodeArticle_WorstCase measures throughput with ~1/64 escape
// density (all 256 byte values present), which forces more special-byte
// handling than a typical article.
func BenchmarkDecodeArticle_WorstCase(b *testing.B) {
	b.SetBytes(int64(benchSize))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, _, _, _, err := DecodeArticle(benchWorstCase)
		if err != nil {
			b.Fatal(err)
		}
		if len(data) == 0 {
			b.Fatal("empty result")
		}
	}
}

// BenchmarkDecodeBody_BestCase isolates the hot decode loop from header/trailer
// parsing and CRC computation, for micro-level throughput measurement.
func BenchmarkDecodeBody_BestCase(b *testing.B) {
	raw := bestCaseRaw(benchSize)
	encoded := make([]byte, 0, benchSize)
	for _, c := range raw {
		enc := byte((int(c) + 42) % 256)
		encoded = append(encoded, enc)
	}
	want := crc32.ChecksumIEEE(raw)
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, gotCRC := decodeBody(encoded, int64(len(raw)))
		if gotCRC != want {
			b.Fatalf("CRC mismatch: 0x%08x != 0x%08x", gotCRC, want)
		}
		if len(data) != len(raw) {
			b.Fatal("length mismatch")
		}
	}
}
