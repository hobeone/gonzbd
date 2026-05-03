package decoder

import (
	"bytes"
	"testing"
)

func TestLatin1ToUTF8_PureASCII(t *testing.T) {
	input := []byte("hello.mkv")
	expected := "hello.mkv"
	if got := latin1ToUTF8(input); got != expected {
		t.Errorf("latin1ToUTF8(%q) = %q, want %q", input, got, expected)
	}
}

func TestLatin1ToUTF8_Accented(t *testing.T) {
	input := []byte{0x47, 0xFC, 0x6E, 0x74, 0x65, 0x72} // "G" + ü + "nter"
	expected := "Günter"
	if got := latin1ToUTF8(input); got != expected {
		t.Errorf("latin1ToUTF8(%x) = %q, want %q", input, got, expected)
	}
}

func TestLatin1ToUTF8_AllHigh(t *testing.T) {
	input := []byte{0xE9, 0xE8, 0xF1} // éèñ
	expected := "éèñ"
	if got := latin1ToUTF8(input); got != expected {
		t.Errorf("latin1ToUTF8(%x) = %q, want %q", input, got, expected)
	}
}

func TestDecodeArticle_Latin1Name(t *testing.T) {
	// Construct a full yEnc article with a Latin1 filename.
	// Filename: "Günter.mkv" (G=0x47, ü=0xFC, nter.mkv)
	latin1Name := []byte{0x47, 0xFC, 0x6E, 0x74, 0x65, 0x72, 0x2E, 0x6D, 0x6B, 0x76}

	// Create article body
	var body bytes.Buffer
	body.WriteString("=ybegin line=128 size=4 name=")
	body.Write(latin1Name)
	body.WriteString("\r\n")
	body.WriteString("test\r\n")

	// yEnc checksum for "test" (ignoring the CRLF)
	// data is "test" - but actually yEnc shifts by 42, let's just make it simple.
	body.WriteString("=yend size=4\r\n")

	encodedData := []byte{'t' + 42, 'e' + 42, 's' + 42, 't' + 42, '\r', '\n'}

	var body2 bytes.Buffer
	body2.WriteString("=ybegin line=128 size=4 name=")
	body2.Write(latin1Name)
	body2.WriteString("\r\n")
	body2.Write(encodedData)
	body2.WriteString("=yend size=4\r\n")

	art, err := DecodeArticle(body2.Bytes())
	if err != nil {
		t.Fatalf("DecodeArticle failed: %v", err)
	}

	expectedName := "Günter.mkv"
	if art.Filename != expectedName {
		t.Errorf("Filename = %q, want %q", art.Filename, expectedName)
	}
}
