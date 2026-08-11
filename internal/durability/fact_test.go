package durability

import "testing"

// TestArticleFact_HasCRCDistinctFromZeroCRC pins the distinction the doc
// comment calls out under R23: a UU-encoded article has HasCRC == false and
// CRC32 == 0, which must remain distinguishable from a yEnc article whose
// CRC32 genuinely happens to be 0. Both are representable, and the zero
// value of ArticleFact must not be mistaken for "verified zero CRC".
func TestArticleFact_HasCRCDistinctFromZeroCRC(t *testing.T) {
	noCRC := ArticleFact{FileIdx: 1, ArtIdx: 2, Offset: 100, Length: 50}
	if noCRC.HasCRC {
		t.Fatal("zero-value ArticleFact.HasCRC should be false")
	}
	if noCRC.CRC32 != 0 {
		t.Fatalf("zero-value ArticleFact.CRC32 = %d, want 0", noCRC.CRC32)
	}

	zeroCRC := ArticleFact{FileIdx: 1, ArtIdx: 3, Offset: 150, Length: 50, CRC32: 0, HasCRC: true}
	if !zeroCRC.HasCRC {
		t.Fatal("HasCRC should be true even though CRC32 == 0")
	}
	if noCRC.HasCRC == zeroCRC.HasCRC {
		t.Fatal("HasCRC must distinguish 'no CRC' from 'CRC is genuinely 0'")
	}
}
