// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hobeone/gonzbd/internal/decoder"
	"github.com/hobeone/gonzbd/test/mocknntp"
)

func TestParseFaultSpec_Success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		spec     string
		expected FaultRule
	}{
		{
			name: "drop with file match and parts",
			spec: "drop:part03.rar:1,2",
			expected: FaultRule{
				Action:    FaultDrop,
				FileMatch: "part03.rar",
				Parts:     []int{1, 2},
			},
		},
		{
			name: "corrupt payload with file, part, bytes, and max",
			spec: "corrupt-payload:part02.rar:1:bytes=32:max=2",
			expected: FaultRule{
				Action:       FaultCorruptPayload,
				FileMatch:    "part02.rar",
				Parts:        []int{1},
				CorruptBytes: 32,
				MaxFires:     2,
			},
		},
		{
			name: "drop mid body with role, rate, and max",
			spec: "drop-mid-body:role=rar_volume:rate=0.25:max=3",
			expected: FaultRule{
				Action:   FaultDropMidBody,
				Role:     RoleRARVolume,
				Rate:     0.25,
				MaxFires: 3,
			},
		},
		{
			name: "stall with msgid and timeout",
			spec: "stall:msgid=abc@test:timeout=100ms",
			expected: FaultRule{
				Action:       FaultStall,
				MessageIDs:   []string{"abc@test"},
				TimeoutAfter: 100 * time.Millisecond,
			},
		},
		{
			name: "corrupt wire with explicit kv fields",
			spec: "corrupt-wire:file=test.par2:parts=3,4:bytes=8",
			expected: FaultRule{
				Action:       FaultCorruptWire,
				FileMatch:    "test.par2",
				Parts:        []int{3, 4},
				CorruptBytes: 8,
			},
		},
		{
			name: "file match positional only",
			spec: "drop:sample.mkv",
			expected: FaultRule{
				Action:    FaultDrop,
				FileMatch: "sample.mkv",
			},
		},
		{
			name: "multiple message IDs via kv",
			spec: "stall:msgid=m1@test,<m2@test>",
			expected: FaultRule{
				Action:     FaultStall,
				MessageIDs: []string{"m1@test", "m2@test"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rule, err := ParseFaultSpec(tt.spec)
			if err != nil {
				t.Fatalf("ParseFaultSpec(%q) unexpected error: %v", tt.spec, err)
			}
			if rule.Action != tt.expected.Action {
				t.Errorf("Action = %v, want %v", rule.Action, tt.expected.Action)
			}
			if rule.FileMatch != tt.expected.FileMatch {
				t.Errorf("FileMatch = %q, want %q", rule.FileMatch, tt.expected.FileMatch)
			}
			if rule.Role != tt.expected.Role {
				t.Errorf("Role = %v, want %v", rule.Role, tt.expected.Role)
			}
			if len(rule.Parts) != len(tt.expected.Parts) {
				t.Errorf("Parts = %v, want %v", rule.Parts, tt.expected.Parts)
			} else {
				for i := range rule.Parts {
					if rule.Parts[i] != tt.expected.Parts[i] {
						t.Errorf("Parts[%d] = %d, want %d", i, rule.Parts[i], tt.expected.Parts[i])
					}
				}
			}
			if len(rule.MessageIDs) != len(tt.expected.MessageIDs) {
				t.Errorf("MessageIDs = %v, want %v", rule.MessageIDs, tt.expected.MessageIDs)
			} else {
				for i := range rule.MessageIDs {
					if rule.MessageIDs[i] != tt.expected.MessageIDs[i] {
						t.Errorf("MessageIDs[%d] = %q, want %q", i, rule.MessageIDs[i], tt.expected.MessageIDs[i])
					}
				}
			}
			if rule.Rate != tt.expected.Rate {
				t.Errorf("Rate = %v, want %v", rule.Rate, tt.expected.Rate)
			}
			if rule.CorruptBytes != tt.expected.CorruptBytes {
				t.Errorf("CorruptBytes = %d, want %d", rule.CorruptBytes, tt.expected.CorruptBytes)
			}
			if rule.TimeoutAfter != tt.expected.TimeoutAfter {
				t.Errorf("TimeoutAfter = %v, want %v", rule.TimeoutAfter, tt.expected.TimeoutAfter)
			}
			if rule.MaxFires != tt.expected.MaxFires {
				t.Errorf("MaxFires = %d, want %d", rule.MaxFires, tt.expected.MaxFires)
			}
		})
	}
}

func TestParseFaultSpec_Errors(t *testing.T) {
	t.Parallel()

	invalidSpecs := []string{
		"",
		"unknown-action:file.rar",
		"drop",                // missing selector or rate
		"drop:part03.rar:abc", // malformed parts
		"drop:rate=notafloat",
		"drop:rate=0.0",
		"drop:rate=1.5",
		"drop:rate=-0.5",
		"corrupt-payload:part.rar:bytes=abc",
		"corrupt-payload:part.rar:bytes=-1",
		"drop:part.rar:max=abc",
		"drop:part.rar:max=-1",
		"stall:msgid=a@b:timeout=invalid",
		"drop:unknown_key=val",
		"drop:a:1,2:extra_positional",
	}

	for _, spec := range invalidSpecs {
		t.Run("invalid_"+spec, func(t *testing.T) {
			t.Parallel()
			_, err := ParseFaultSpec(spec)
			if err == nil {
				t.Errorf("ParseFaultSpec(%q) expected error, got nil", spec)
			}
		})
	}
}

func TestFaultEngine_SelectorsAndMatching(t *testing.T) {
	t.Parallel()

	engine := NewFaultEngine(42, []FaultRule{
		{
			Action:    FaultDrop,
			FileMatch: "part01.rar",
			Parts:     []int{1, 2},
		},
		{
			Action:     FaultCorruptWire,
			MessageIDs: []string{"msg-special@test"},
		},
		{
			Action: FaultDropMidBody,
			Role:   RoleSidecar,
		},
	})

	// Match first rule (FileMatch + Parts)
	matched, ok := engine.Match(ArticleTarget{
		MessageID:   "m1@test",
		DiskName:    "release.part01.rar",
		NZBFilename: "release.part01.rar",
		Role:        RoleRARVolume,
		PartNum:     1,
	})
	if !ok || matched.Action != FaultDrop {
		t.Fatalf("expected FaultDrop match, got ok=%v action=%v", ok, matched.Action)
	}

	// DiskName doesn't match part01.rar, but MessageID matches second rule
	matched, ok = engine.Match(ArticleTarget{
		MessageID:   "msg-special@test",
		DiskName:    "release.part02.rar",
		NZBFilename: "release.part02.rar",
		Role:        RoleRARVolume,
		PartNum:     1,
	})
	if !ok || matched.Action != FaultCorruptWire {
		t.Fatalf("expected FaultCorruptWire match, got ok=%v action=%v", ok, matched.Action)
	}
	if matched.CorruptBytes != 16 {
		t.Errorf("CorruptBytes default = %d, want 16", matched.CorruptBytes)
	}

	// Match by Role (RoleSidecar)
	matched, ok = engine.Match(ArticleTarget{
		MessageID:   "sidecar@test",
		DiskName:    "release.nfo",
		NZBFilename: "release.nfo",
		Role:        RoleSidecar,
		PartNum:     1,
	})
	if !ok || matched.Action != FaultDropMidBody {
		t.Fatalf("expected FaultDropMidBody match, got ok=%v action=%v", ok, matched.Action)
	}

	// NZBFilename match
	engine.AddRule(FaultRule{
		Action:    FaultStall,
		FileMatch: "hidden.bin",
	})
	matched, ok = engine.Match(ArticleTarget{
		MessageID:   "hidden@test",
		DiskName:    "obfuscated.xyz",
		NZBFilename: "hidden.bin",
		Role:        RoleRARVolume,
		PartNum:     1,
	})
	if !ok || matched.Action != FaultStall {
		t.Fatalf("expected FaultStall match on NZBFilename, got ok=%v action=%v", ok, matched.Action)
	}
	if matched.TimeoutAfter != 30*time.Second {
		t.Errorf("TimeoutAfter default = %v, want 30s", matched.TimeoutAfter)
	}

	// Multi-part rule with PartNum mismatch
	engine.AddRule(FaultRule{
		Action: FaultDrop,
		Parts:  []int{99},
	})
	_, ok = engine.Match(ArticleTarget{
		MessageID: "p99@test",
		DiskName:  "p99.rar",
		PartNum:   1,
	})
	if ok {
		t.Errorf("expected no match when target.PartNum does not match rule.Parts")
	}

	// No match
	_, ok = engine.Match(ArticleTarget{
		MessageID:   "nomatch@test",
		DiskName:    "release.part03.rar",
		NZBFilename: "release.part03.rar",
		Role:        RoleRARVolume,
		PartNum:     5,
	})
	if ok {
		t.Errorf("expected no match, but rule matched")
	}
}

func TestFaultEngine_MaxFires(t *testing.T) {
	t.Parallel()

	engine := NewFaultEngine(1, []FaultRule{
		{
			Action:    FaultDrop,
			FileMatch: "exhaust.rar",
			MaxFires:  2,
		},
	})

	target := ArticleTarget{
		MessageID: "test@test",
		DiskName:  "exhaust.rar",
		PartNum:   1,
	}

	// Fire 1
	if _, ok := engine.Match(target); !ok {
		t.Fatalf("fire 1 expected match")
	}
	// Fire 2
	if _, ok := engine.Match(target); !ok {
		t.Fatalf("fire 2 expected match")
	}
	// Fire 3: should be skipped because MaxFires reached
	if _, ok := engine.Match(target); ok {
		t.Fatalf("fire 3 expected no match due to MaxFires exhaustion")
	}

	rules := engine.Rules()
	if len(rules) != 1 || rules[0].Fired != 2 {
		t.Fatalf("expected Fired == 2, got %v", rules)
	}

	// Multi-part rule with MaxFires=1 caps per-article (MessageID) so both part 1 and part 2 fire once each.
	perArtEngine := NewFaultEngine(1, []FaultRule{
		{
			Action:    FaultDrop,
			FileMatch: "part01.rar",
			Parts:     []int{1, 2},
			MaxFires:  1,
		},
	})
	p1 := ArticleTarget{MessageID: "seg1@test", DiskName: "part01.rar", PartNum: 1}
	p2 := ArticleTarget{MessageID: "seg2@test", DiskName: "part01.rar", PartNum: 2}
	if _, ok := perArtEngine.Match(p1); !ok {
		t.Fatal("expected part 1 first request to match")
	}
	if _, ok := perArtEngine.Match(p1); ok {
		t.Fatal("expected part 1 second request to be skipped after max=1")
	}
	if _, ok := perArtEngine.Match(p2); !ok {
		t.Fatal("expected part 2 first request to match independently under max=1")
	}
	if _, ok := perArtEngine.Match(p2); ok {
		t.Fatal("expected part 2 second request to be skipped after max=1")
	}

	// MatchAction only evaluates and spends budget on allowed actions.
	statSafeEngine := NewFaultEngine(1, []FaultRule{
		{
			Action:    FaultCorruptPayload,
			FileMatch: "part02.rar",
			MaxFires:  1,
		},
	})
	p3 := ArticleTarget{MessageID: "seg3@test", DiskName: "part02.rar", PartNum: 1}
	if _, ok := statSafeEngine.MatchAction(p3, FaultDrop); ok {
		t.Fatal("expected MatchAction(FaultDrop) not to match FaultCorruptPayload")
	}
	if _, ok := statSafeEngine.Match(p3); !ok {
		t.Fatal("expected subsequent Match() to still have its MaxFires=1 budget")
	}

	// Whole-file rule without parts=/msgid= (e.g. drop:part01.rar:max=1) fires once in total across the file.
	wholeFileRule, err := ParseFaultSpec("drop:part01.rar:max=1")
	if err != nil {
		t.Fatalf("ParseFaultSpec: %v", err)
	}
	wholeFileEngine := NewFaultEngine(1, []FaultRule{wholeFileRule})
	if _, ok := wholeFileEngine.Match(p1); !ok {
		t.Fatal("expected whole-file max=1 rule to fire on first segment")
	}
	if _, ok := wholeFileEngine.Match(p2); ok {
		t.Fatal("expected whole-file max=1 rule NOT to fire on second segment")
	}
}

func TestValidateFaultRule(t *testing.T) {
	t.Parallel()
	if err := ValidateFaultRule(nil); err == nil {
		t.Error("expected error for nil rule")
	}
	badRules := []FaultRule{
		{Action: "bogus", FileMatch: "a"},
		{Action: FaultDrop, Role: "bogus_role"},
		{Action: FaultDrop, Parts: []int{0}},
		{Action: FaultDrop, Rate: -0.1},
		{Action: FaultDrop, Rate: 1.5},
		{Action: FaultCorruptPayload, FileMatch: "a", CorruptBytes: -1},
		{Action: FaultStall, FileMatch: "a", TimeoutAfter: -time.Second},
		{Action: FaultDrop, FileMatch: "a", MaxFires: -1},
		{Action: FaultDrop}, // no selector
	}
	for i := range badRules {
		if err := ValidateFaultRule(&badRules[i]); err == nil {
			t.Errorf("case %d: expected error for %+v", i, badRules[i])
		}
	}
}

func TestFaultEngine_RulesCopyAndClear(t *testing.T) {
	t.Parallel()

	engine := NewFaultEngine(1, []FaultRule{
		{
			Action:     FaultDrop,
			MessageIDs: []string{"orig@test"},
			Parts:      []int{1},
		},
	})

	rules := engine.Rules()
	// Mutate returned copy
	rules[0].Action = FaultStall
	rules[0].MessageIDs[0] = "mutated@test"
	rules[0].Parts[0] = 999

	rules2 := engine.Rules()
	if rules2[0].Action != FaultDrop || rules2[0].MessageIDs[0] != "orig@test" || rules2[0].Parts[0] != 1 {
		t.Fatalf("engine rules were mutated through returned copy: %+v", rules2[0])
	}

	engine.ClearRules()
	if len(engine.Rules()) != 0 {
		t.Fatalf("expected 0 rules after ClearRules, got %d", len(engine.Rules()))
	}
}

func TestFaultEngine_Rate(t *testing.T) {
	t.Parallel()

	// Deterministic seed 12345
	engine := NewFaultEngine(12345, []FaultRule{
		{
			Action: FaultDrop,
			Rate:   0.5,
		},
	})

	target := ArticleTarget{
		MessageID: "rate@test",
		DiskName:  "file.rar",
		PartNum:   1,
	}

	matchedCount := 0
	const iterations = 1000
	for range iterations {
		if _, ok := engine.Match(target); ok {
			matchedCount++
		}
	}

	// For rate 0.5 with seed 12345 across 1000 iterations, matchedCount should be close to 500
	if matchedCount < 400 || matchedCount > 600 {
		t.Fatalf("rate 0.5 matchedCount=%d out of %d, expected ~500", matchedCount, iterations)
	}
}

func TestFaultEngine_RuleWithoutSelectorSkipped(t *testing.T) {
	t.Parallel()

	engine := NewFaultEngine(1, []FaultRule{
		{
			Action: FaultDrop,
			// No selectors set, Rate = 0
		},
	})

	_, ok := engine.Match(ArticleTarget{
		MessageID: "test@test",
		DiskName:  "test.rar",
		PartNum:   1,
	})
	if ok {
		t.Fatalf("expected rule without selector to be skipped")
	}
}

func TestCorruptPayloadSlice(t *testing.T) {
	t.Parallel()

	orig := []byte("The quick brown fox jumps over the lazy dog 1234567890! Exactly 64 bytes.")
	corrupted := CorruptPayloadSlice(orig, 16)

	if len(corrupted) != len(orig) {
		t.Fatalf("len(corrupted) = %d, want %d", len(corrupted), len(orig))
	}
	if bytes.Equal(corrupted, orig) {
		t.Fatalf("expected corrupted slice to differ from orig")
	}

	// Multi-part encoding with corrupted payload: valid CRC trailer matching corrupted data
	encoded := mocknntp.EncodeYEncPart("test.bin", 1, 1, int64(len(corrupted)), 0, corrupted)
	art, err := decoder.DecodeArticle(encoded)
	if err != nil {
		t.Fatalf("decoder.DecodeArticle on CorruptPayloadSlice failed: %v", err)
	}
	if bytes.Equal(art.Data, orig) {
		t.Fatalf("decoded data unexpectedly equals original uncorrupted data")
	}
	if !bytes.Equal(art.Data, corrupted) {
		t.Fatalf("decoded data does not match corrupted payload")
	}

	// Single-part encoding
	encodedSingle := mocknntp.EncodeYEnc("single.bin", corrupted)
	artSingle, errSingle := decoder.DecodeArticle(encodedSingle)
	if errSingle != nil {
		t.Fatalf("decoder.DecodeArticle on single-part CorruptPayloadSlice failed: %v", errSingle)
	}
	if !bytes.Equal(artSingle.Data, corrupted) {
		t.Fatalf("decoded single-part data does not match corrupted payload")
	}

	// Edge cases: empty slice
	empty := CorruptPayloadSlice(nil, 16)
	if len(empty) != 0 {
		t.Fatalf("expected empty slice from nil raw")
	}

	// CorruptBytes default and exceeding length
	short := []byte("short")
	shortCorrupted := CorruptPayloadSlice(short, -1)
	if bytes.Equal(shortCorrupted, short) {
		t.Fatalf("expected short slice to be corrupted")
	}
}

func TestCorruptWireBody(t *testing.T) {
	t.Parallel()

	// 1. Multi-part article
	payload := []byte("CorruptWireBody test payload with multiple lines and content that triggers yenc wrapping.")
	encoded := mocknntp.EncodeYEncPart("wiretest.bin", 1, 1, int64(len(payload)), 0, payload)

	corruptedBody := CorruptWireBody(encoded, 8)

	if bytes.Equal(corruptedBody, encoded) {
		t.Fatalf("expected corruptedBody to differ from encoded")
	}

	// Must verify that headers and trailers were preserved
	if !strings.HasPrefix(string(corruptedBody), "=ybegin") {
		t.Fatalf("corrupted body missing =ybegin")
	}
	if !strings.Contains(string(corruptedBody), "=ypart") {
		t.Fatalf("corrupted body missing =ypart")
	}
	if !strings.Contains(string(corruptedBody), "=yend") {
		t.Fatalf("corrupted body missing =yend")
	}

	// Crucial property: decoder.DecodeArticle must fail with decoder.ErrCRCMismatch
	_, err := decoder.DecodeArticle(corruptedBody)
	if err == nil {
		t.Fatalf("expected decoder.DecodeArticle to fail on wire corruption, but succeeded")
	}
	if !errors.Is(err, decoder.ErrCRCMismatch) {
		t.Fatalf("expected ErrCRCMismatch, got %v", err)
	}

	// 2. Single-part article with default n <= 0 and escape characters
	escapedPayload := []byte("hello\x00world\r\n=more\xd6data\x00end")
	encodedSingle := mocknntp.EncodeYEnc("single.bin", escapedPayload)
	corruptedSingle := CorruptWireBody(encodedSingle, -1)
	if bytes.Equal(corruptedSingle, encodedSingle) {
		t.Fatalf("expected corruptedSingle to differ from encodedSingle")
	}
	_, errSingle := decoder.DecodeArticle(corruptedSingle)
	if !errors.Is(errSingle, decoder.ErrCRCMismatch) {
		t.Fatalf("expected ErrCRCMismatch on single-part article, got %v", errSingle)
	}

	// 3. Edge cases: empty or malformed wire bodies
	empty := CorruptWireBody(nil, 16)
	if len(empty) != 0 {
		t.Fatalf("expected empty slice from nil wire body")
	}

	notYenc := []byte("not yenc data at all")
	if !bytes.Equal(CorruptWireBody(notYenc, 16), notYenc) {
		t.Fatalf("expected unmodified output when missing =ybegin")
	}

	noNLBegin := []byte("=ybegin line=128 size=10 name=foo")
	if !bytes.Equal(CorruptWireBody(noNLBegin, 16), noNLBegin) {
		t.Fatalf("expected unmodified output when =ybegin lacks newline")
	}

	noNLPart := []byte("=ybegin line=128 size=10 name=foo\n=ypart begin=1 end=10")
	if !bytes.Equal(CorruptWireBody(noNLPart, 16), noNLPart) {
		t.Fatalf("expected unmodified output when =ypart lacks newline")
	}

	noTrailer := []byte("=ybegin line=128 size=10 name=foo\nhello world\n")
	if !bytes.Equal(CorruptWireBody(noTrailer, 16), noTrailer) {
		t.Fatalf("expected unmodified output when lacking =yend")
	}

	emptyData := []byte("=ybegin line=128 size=0 name=foo\n=yend size=0\n")
	if !bytes.Equal(CorruptWireBody(emptyData, 16), emptyData) {
		t.Fatalf("expected unmodified output when no data section exists")
	}

	noCandidates := []byte("=ybegin line=128 size=0 name=foo\n\r\n=yend size=0\n")
	if !bytes.Equal(CorruptWireBody(noCandidates, 16), noCandidates) {
		t.Fatalf("expected unmodified output when no candidate data bytes exist")
	}
}

func TestRateLimiter(t *testing.T) {
	t.Parallel()

	// 1. Unlimited (0 MB/s) returns immediately
	rl := NewRateLimiter(0)
	if rl.RateMBps() != 0 {
		t.Errorf("expected 0 rate, got %v", rl.RateMBps())
	}
	ctx := context.Background()
	start := time.Now()
	if err := rl.Wait(ctx, 1024*1024); err != nil {
		t.Fatalf("rl.Wait unexpected error: %v", err)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Errorf("unlimited rate took too long: %v", time.Since(start))
	}

	// 2. Negative bytes or negative rate
	if err := rl.Wait(ctx, -100); err != nil {
		t.Fatalf("rl.Wait(-100) unexpected error: %v", err)
	}
	rl.SetRateMBps(-5.0)
	if err := rl.Wait(ctx, 1024); err != nil {
		t.Fatalf("rl.Wait negative rate unexpected error: %v", err)
	}

	// 3. Pacing with 20 MB/s (1 MB should take ~50ms)
	rl.SetRateMBps(20.0)
	if rl.RateMBps() != 20.0 {
		t.Errorf("RateMBps() = %v, want 20.0", rl.RateMBps())
	}

	start = time.Now()
	// 500 KB at 20 MB/s = 25ms delay
	if err := rl.Wait(ctx, 500*1000); err != nil {
		t.Fatalf("rl.Wait paced error: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Errorf("paced wait too short: %v, expected at least 20ms", elapsed)
	}

	// 4. Context cancellation (pre-canceled and mid-wait refund)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := rl.Wait(cancelCtx, 500*1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	rl.SetRateMBps(1.0) // 1 MB/s -> 500 KB is 500ms
	midCtx, midCancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer midCancel()
	err = rl.Wait(midCtx, 500*1000)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded during wait, got %v", err)
	}
	rl.SetRateMBps(20.0)

	// 5. Concurrency
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			_ = rl.Wait(context.Background(), 10000)
		})
	}
	wg.Wait()
}
