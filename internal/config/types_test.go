package config

import (
	"encoding/json"
	"testing"
)

// ---------- ByteSize ----------

func TestByteSize_String(t *testing.T) {
	t.Parallel()
	tests := []struct {
		b    ByteSize
		want string
	}{
		{0, "0"},
		{1024, "1K"},
		{1048576, "1M"},
		{1073741824, "1G"},
		{1099511627776, "1T"},
		{1500, "1500"}, // not evenly divisible
	}
	for _, tt := range tests {
		got := tt.b.String()
		if got != tt.want {
			t.Errorf("ByteSize(%d).String() = %q, want %q", tt.b, got, tt.want)
		}
	}
}

func TestByteSize_MarshalJSON(t *testing.T) {
	t.Parallel()
	b := ByteSize(1048576)
	data, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(data) != `"1M"` {
		t.Errorf("got %s, want \"1M\"", data)
	}
}

func TestByteSize_UnmarshalJSON_String(t *testing.T) {
	t.Parallel()
	var b ByteSize
	if err := b.UnmarshalJSON([]byte(`"10M"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if int64(b) != 10*1048576 {
		t.Errorf("got %d, want %d", b, 10*1048576)
	}
}

func TestByteSize_UnmarshalJSON_Number(t *testing.T) {
	t.Parallel()
	var b ByteSize
	if err := b.UnmarshalJSON([]byte("12345")); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if int64(b) != 12345 {
		t.Errorf("got %d, want 12345", b)
	}
}

func TestByteSize_UnmarshalJSON_Invalid(t *testing.T) {
	t.Parallel()
	var b ByteSize
	if err := b.UnmarshalJSON([]byte("not-a-number")); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseByteSize_Unlimited(t *testing.T) {
	t.Parallel()
	b, err := ParseByteSize("unlimited")
	if err != nil {
		t.Fatalf("ParseByteSize: %v", err)
	}
	if b != 0 {
		t.Errorf("got %d, want 0 for unlimited", b)
	}
}

func TestParseByteSize_Empty(t *testing.T) {
	t.Parallel()
	b, err := ParseByteSize("")
	if err != nil {
		t.Fatalf("ParseByteSize: %v", err)
	}
	if b != 0 {
		t.Errorf("got %d, want 0 for empty", b)
	}
}

func TestParseByteSize_Negative(t *testing.T) {
	t.Parallel()
	_, err := ParseByteSize("-100")
	if err == nil {
		t.Error("expected error for negative value")
	}
}

func TestParseByteSize_NegativeWithSuffix(t *testing.T) {
	t.Parallel()
	_, err := ParseByteSize("-1G")
	if err == nil {
		t.Error("expected error for negative value with suffix")
	}
}

func TestParseByteSize_MissingSuffix(t *testing.T) {
	t.Parallel()
	_, err := ParseByteSize("M")
	if err == nil {
		t.Error("expected error for suffix without number")
	}
}

func TestParseByteSize_Fractional(t *testing.T) {
	t.Parallel()
	b, err := ParseByteSize("1.5G")
	if err != nil {
		t.Fatalf("ParseByteSize: %v", err)
	}
	want := ByteSize(1.5 * 1073741824)
	if b != want {
		t.Errorf("got %d, want %d", b, want)
	}
}

// ---------- Percent ----------

func TestPercent_MarshalJSON(t *testing.T) {
	t.Parallel()
	p := Percent(50)
	data, err := p.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(data) != "50" {
		t.Errorf("got %s, want 50", data)
	}
}

func TestPercent_UnmarshalJSON_Valid(t *testing.T) {
	t.Parallel()
	var p Percent
	if err := json.Unmarshal([]byte("75"), &p); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if p != 75 {
		t.Errorf("got %d, want 75", p)
	}
}

func TestPercent_UnmarshalJSON_OutOfRange(t *testing.T) {
	t.Parallel()
	var p Percent
	if err := p.UnmarshalJSON([]byte("150")); err == nil {
		t.Error("expected error for 150")
	}
}

func TestPercent_UnmarshalJSON_Negative(t *testing.T) {
	t.Parallel()
	var p Percent
	if err := p.UnmarshalJSON([]byte("-1")); err == nil {
		t.Error("expected error for -1")
	}
}

func TestPercent_UnmarshalJSON_InvalidText(t *testing.T) {
	t.Parallel()
	var p Percent
	if err := p.UnmarshalJSON([]byte("abc")); err == nil {
		t.Error("expected error for non-integer")
	}
}

// ---------- SSLVerify ----------

func TestSSLVerify_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v       SSLVerify
		wantErr bool
	}{
		{SSLVerifyNone, false},
		{SSLVerifyMinimal, false},
		{SSLVerifyHostname, false},
		{SSLVerifyStrict, false},
		{SSLVerify(-1), true},
		{SSLVerify(4), true},
	}
	for _, tt := range tests {
		err := tt.v.Validate()
		if (err != nil) != tt.wantErr {
			t.Errorf("SSLVerify(%d).Validate() = %v, wantErr = %v", tt.v, err, tt.wantErr)
		}
	}
}

// ---------- SanitizeOptions ----------

func TestSanitizeOptions(t *testing.T) {
	t.Parallel()
	dc := DownloadConfig{
		ReplaceIllegalWith: "_",
		ReplaceSpacesWith:  "-",
		StripDiacritics:    true,
		CleanupList:        []string{"sample"},
	}
	opts := dc.SanitizeOptions()
	if opts.ReplaceIllegalWith != "_" {
		t.Errorf("ReplaceIllegalWith = %q", opts.ReplaceIllegalWith)
	}
	if opts.ReplaceSpacesWith != "-" {
		t.Errorf("ReplaceSpacesWith = %q", opts.ReplaceSpacesWith)
	}
	if !opts.StripDiacritics {
		t.Error("StripDiacritics should be true")
	}
	if len(opts.CleanupList) != 1 {
		t.Errorf("CleanupList len = %d, want 1", len(opts.CleanupList))
	}
}
