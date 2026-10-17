package decoder

import "testing"

func TestBufferClass_Table(t *testing.T) {
	tests := []struct {
		n        int64
		wantIdx  int
		wantSize int64
	}{
		{1, 0, 16 << 10},
		{16 << 10, 0, 16 << 10},
		{16<<10 + 1, 1, 20 << 10},
		{32 << 10, 4, 32 << 10},
		{32<<10 + 1, 5, 40 << 10},
		{64 << 10, 8, 64 << 10},
		{66 << 10, 9, 80 << 10},
		{512 << 10, 20, 512 << 10},
		{768000, 22, 768 << 10},
		{maxDecodeSize, numBufferClasses - 1, maxDecodeSize},
	}
	for _, tt := range tests {
		idx, size := bufferClass(tt.n)
		if idx != tt.wantIdx || size != tt.wantSize {
			t.Errorf("bufferClass(%d) = (%d, %d), want (%d, %d)", tt.n, idx, size, tt.wantIdx, tt.wantSize)
		}
	}
}

// TestBufferClass_Properties checks every class boundary up to maxDecodeSize:
// a class covers its request, wastes at most a quarter of it, and maps to one
// pool index, with sizes strictly increasing by index.
func TestBufferClass_Properties(t *testing.T) {
	sizeOf := make(map[int]int64)
	check := func(n int64) {
		idx, size := bufferClass(n)
		if idx < 0 || idx >= numBufferClasses {
			t.Fatalf("bufferClass(%d) index %d outside [0, %d)", n, idx, numBufferClasses)
		}
		if size < n {
			t.Fatalf("bufferClass(%d) size %d is smaller than the request", n, size)
		}
		if n > minBufferClass && size > n+n/4 {
			t.Fatalf("bufferClass(%d) size %d wastes more than a quarter", n, size)
		}
		if prev, ok := sizeOf[idx]; ok && prev != size {
			t.Fatalf("index %d maps to both %d and %d", idx, prev, size)
		}
		sizeOf[idx] = size
	}
	for n := int64(1); n <= maxDecodeSize; {
		_, size := bufferClass(n)
		check(n)
		check(size)
		if size < maxDecodeSize {
			check(size + 1)
		}
		n = size + 1
	}
	if len(sizeOf) != numBufferClasses {
		t.Fatalf("%d distinct classes, numBufferClasses is %d", len(sizeOf), numBufferClasses)
	}
	for i := 1; i < numBufferClasses; i++ {
		if sizeOf[i] <= sizeOf[i-1] {
			t.Fatalf("class %d size %d is not above class %d size %d", i, sizeOf[i], i-1, sizeOf[i-1])
		}
	}
}

func TestGetBuffer_CapacityIsBufferCap(t *testing.T) {
	for _, req := range []int64{-1, 0, 100, 64 << 10, 66 << 10, 768000, maxDecodeSize, maxDecodeSize + 1} {
		buf := GetBuffer(req)
		if int64(cap(buf)) != BufferCap(req) || len(buf) != 0 {
			t.Errorf("GetBuffer(%d): len %d cap %d, want len 0 cap %d", req, len(buf), cap(buf), BufferCap(req))
		}
		if req > 0 && req <= maxDecodeSize && int64(cap(buf)) < req {
			t.Errorf("GetBuffer(%d): cap %d below the request", req, cap(buf))
		}
		PutBuffer(buf)
	}
}

// TestPutBuffer_DropsOffClassCapacity pins that a pooled buffer always has its
// class's exact capacity: a buffer of any other capacity put back must not be
// handed out by GetBuffer for that class.
func TestPutBuffer_DropsOffClassCapacity(t *testing.T) {
	odd := make([]byte, 0, 70<<10) // between the 64 KiB and 80 KiB classes
	for range 100 {
		PutBuffer(odd)
	}
	for range 100 {
		if got := cap(GetBuffer(66 << 10)); got != 80<<10 {
			t.Fatalf("GetBuffer(66 KiB) cap = %d, want %d", got, 80<<10)
		}
	}
}
