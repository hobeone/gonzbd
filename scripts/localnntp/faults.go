// Package main implements the localnntp release generator, NNTP server, and crash verifier.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FaultAction represents the type of fault to inject on an NNTP request.
type FaultAction string

const (
	// FaultDrop returns 430 No Such Article.
	FaultDrop FaultAction = "drop"
	// FaultCorruptPayload corrupts payload bytes while computing a matching valid yEnc CRC trailer.
	FaultCorruptPayload FaultAction = "corrupt-payload"
	// FaultCorruptWire corrupts payload bytes on the wire leaving the original trailer CRC mismatched.
	FaultCorruptWire FaultAction = "corrupt-wire"
	// FaultDropMidBody sends a partial body and abruptly closes the connection.
	FaultDropMidBody FaultAction = "drop-mid-body"
	// FaultStall holds the connection open without sending bytes until timeout.
	FaultStall FaultAction = "stall"
)

// FaultRule defines matching criteria and fault behavior for an article.
type FaultRule struct {
	Action       FaultAction   `json:"action"`
	MessageIDs   []string      `json:"message_ids,omitempty"`
	FileMatch    string        `json:"file_match,omitempty"`
	Role         FileRole      `json:"role,omitempty"`
	Parts        []int         `json:"parts,omitempty"`
	Rate         float64       `json:"rate,omitempty"`
	CorruptBytes int           `json:"corrupt_bytes,omitempty"`
	TimeoutAfter time.Duration `json:"timeout_after,omitempty"`
	MaxFires     int           `json:"max_fires,omitempty"`
	Fired        int           `json:"fired"`
	firedByID    map[string]int
}

// ArticleTarget describes an article segment being requested or processed.
type ArticleTarget struct {
	MessageID   string
	DiskName    string
	NZBFilename string
	Role        FileRole
	PartNum     int
}

// FaultEngine provides thread-safe fault evaluation and rule accounting.
type FaultEngine struct {
	mu    sync.Mutex
	rules []FaultRule
	rng   *rand.Rand
}

func normalizeMessageIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if clean := extractMessageID(id); clean != "" {
			out = append(out, clean)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneFaultRule(r FaultRule) FaultRule {
	c := r
	c.MessageIDs = normalizeMessageIDs(r.MessageIDs)
	c.Parts = slices.Clone(r.Parts)
	c.firedByID = maps.Clone(r.firedByID)
	return c
}

// ValidateFaultRule checks that r has a known action, valid parameter ranges,
// and at least one selector or positive rate. It normalizes MessageIDs and
// resets runtime fire counters.
func ValidateFaultRule(r *FaultRule) error {
	if r == nil {
		return errors.New("fault rule is nil")
	}
	switch r.Action {
	case FaultDrop, FaultCorruptPayload, FaultCorruptWire, FaultDropMidBody, FaultStall:
	default:
		return fmt.Errorf("unknown fault action %q", r.Action)
	}
	if r.Role != "" {
		switch r.Role {
		case RoleRARVolume, RolePar2Index, RolePar2Volume, RoleSidecar:
		default:
			return fmt.Errorf("unknown file role %q", r.Role)
		}
	}
	for _, p := range r.Parts {
		if p <= 0 {
			return fmt.Errorf("part number must be positive, got %d", p)
		}
	}
	if r.Rate < 0 || r.Rate > 1.0 {
		return fmt.Errorf("rate must be in [0, 1], got %f", r.Rate)
	}
	if r.CorruptBytes < 0 {
		return fmt.Errorf("corrupt_bytes cannot be negative: %d", r.CorruptBytes)
	}
	if r.TimeoutAfter < 0 {
		return fmt.Errorf("timeout_after cannot be negative: %v", r.TimeoutAfter)
	}
	if r.MaxFires < 0 {
		return fmt.Errorf("max_fires cannot be negative: %d", r.MaxFires)
	}
	r.MessageIDs = normalizeMessageIDs(r.MessageIDs)
	r.Fired = 0
	r.firedByID = nil
	if len(r.MessageIDs) == 0 && r.FileMatch == "" && r.Role == "" && len(r.Parts) == 0 && r.Rate <= 0 {
		return errors.New("fault rule requires at least one selector (file_match, role, message_ids, parts) or positive rate")
	}
	return nil
}

// NewFaultEngine creates a thread-safe FaultEngine seeded with seed and initial rules.
func NewFaultEngine(seed uint64, rules []FaultRule) *FaultEngine {
	copied := make([]FaultRule, len(rules))
	for i, r := range rules {
		copied[i] = cloneFaultRule(r)
	}
	return &FaultEngine{
		rules: copied,
		rng:   rand.New(rand.NewPCG(seed, 0x9E3779B97F4A7C15)), //nolint:gosec // deterministic seeded PRNG is intentional for reproducible fault injection
	}
}

// Rules returns a deep copy of all rules currently in the engine.
func (e *FaultEngine) Rules() []FaultRule {
	e.mu.Lock()
	defer e.mu.Unlock()

	out := make([]FaultRule, len(e.rules))
	for i, r := range e.rules {
		out[i] = cloneFaultRule(r)
	}
	return out
}

// AddRule appends a fault rule to the engine.
func (e *FaultEngine) AddRule(r FaultRule) {
	e.mu.Lock()
	defer e.mu.Unlock()

	c := cloneFaultRule(r)
	c.Fired = 0
	c.firedByID = nil
	e.rules = append(e.rules, c)
}

// ClearRules removes all fault rules from the engine.
func (e *FaultEngine) ClearRules() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rules = nil
}

// Match evaluates candidate rules in order against target. If a rule matches,
// its Fired counter is incremented and a copy with applied defaults is returned.
func (e *FaultEngine) Match(target ArticleTarget) (FaultRule, bool) {
	return e.MatchAction(target)
}

// MatchAction evaluates candidate rules in order against target, considering
// only rules whose Action is in allowed (or any action when allowed is empty).
// When MaxFires > 0, deterministic selector rules (Rate == 0) cap fires per
// target MessageID when MessageID is non-empty, while probabilistic rules
// (Rate > 0) cap total fires across the rule.
func (e *FaultEngine) MatchAction(target ArticleTarget, allowed ...FaultAction) (FaultRule, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	for i := range e.rules {
		r := &e.rules[i]
		if len(allowed) > 0 && !slices.Contains(allowed, r.Action) {
			continue
		}
		if r.MaxFires > 0 {
			if r.Rate == 0 && target.MessageID != "" {
				if r.firedByID[target.MessageID] >= r.MaxFires {
					continue
				}
			} else if r.Fired >= r.MaxFires {
				continue
			}
		}

		hasSelector := len(r.MessageIDs) > 0 || r.FileMatch != "" || r.Role != "" || len(r.Parts) > 0 || r.Rate > 0
		if !hasSelector {
			continue
		}

		if len(r.MessageIDs) > 0 && !slices.Contains(r.MessageIDs, target.MessageID) {
			continue
		}
		if r.FileMatch != "" && !strings.Contains(target.DiskName, r.FileMatch) && !strings.Contains(target.NZBFilename, r.FileMatch) {
			continue
		}
		if r.Role != "" && target.Role != r.Role {
			continue
		}
		if len(r.Parts) > 0 && !slices.Contains(r.Parts, target.PartNum) {
			continue
		}
		if r.Rate > 0 && e.rng.Float64() >= r.Rate {
			continue
		}

		r.Fired++
		if target.MessageID != "" {
			if r.firedByID == nil {
				r.firedByID = make(map[string]int)
			}
			r.firedByID[target.MessageID]++
		}
		matched := cloneFaultRule(*r)
		if matched.CorruptBytes <= 0 {
			matched.CorruptBytes = 16
		}
		if matched.TimeoutAfter <= 0 {
			matched.TimeoutAfter = 30 * time.Second
		}
		return matched, true
	}

	return FaultRule{}, false
}

// ParseFaultSpec parses shorthand CLI fault specifications into a FaultRule.
func ParseFaultSpec(spec string) (FaultRule, error) {
	if spec == "" {
		return FaultRule{}, errors.New("empty fault spec")
	}

	parts := strings.Split(spec, ":")
	var rule FaultRule

	action := FaultAction(parts[0])
	switch action {
	case FaultDrop, FaultCorruptPayload, FaultCorruptWire, FaultDropMidBody, FaultStall:
		rule.Action = action
	default:
		return FaultRule{}, fmt.Errorf("unknown fault action %q", parts[0])
	}

	pos := 0
	for _, token := range parts[1:] {
		if token == "" {
			continue
		}
		if strings.Contains(token, "=") {
			k, v, _ := strings.Cut(token, "=")
			switch k {
			case "msgid", "message_id":
				ids := strings.Split(v, ",")
				for _, id := range ids {
					if clean := extractMessageID(id); clean != "" {
						rule.MessageIDs = append(rule.MessageIDs, clean)
					}
				}
			case "file", "file_match":
				rule.FileMatch = v
			case "role":
				rule.Role = FileRole(v)
			case "part", "parts":
				nums := strings.Split(v, ",")
				for _, nStr := range nums {
					n, err := strconv.Atoi(nStr)
					if err != nil {
						return FaultRule{}, fmt.Errorf("invalid part number %q: %w", nStr, err)
					}
					rule.Parts = append(rule.Parts, n)
				}
			case "rate":
				r, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return FaultRule{}, fmt.Errorf("invalid rate %q: %w", v, err)
				}
				if r <= 0 || r > 1.0 {
					return FaultRule{}, fmt.Errorf("rate must be in (0, 1], got %f", r)
				}
				rule.Rate = r
			case "bytes", "corrupt_bytes":
				b, err := strconv.Atoi(v)
				if err != nil || b < 0 {
					return FaultRule{}, fmt.Errorf("invalid corrupt bytes %q: %w", v, err)
				}
				rule.CorruptBytes = b
			case "timeout", "timeout_after":
				d, err := time.ParseDuration(v)
				if err != nil {
					return FaultRule{}, fmt.Errorf("invalid timeout %q: %w", v, err)
				}
				rule.TimeoutAfter = d
			case "max", "max_fires":
				m, err := strconv.Atoi(v)
				if err != nil || m < 0 {
					return FaultRule{}, fmt.Errorf("invalid max fires %q: %w", v, err)
				}
				rule.MaxFires = m
			default:
				return FaultRule{}, fmt.Errorf("unknown parameter key %q in fault spec", k)
			}
		} else {
			switch pos {
			case 0:
				rule.FileMatch = token
				pos++
			case 1:
				nums := strings.Split(token, ",")
				for _, nStr := range nums {
					n, err := strconv.Atoi(nStr)
					if err != nil {
						return FaultRule{}, fmt.Errorf("invalid positional parts %q: %w", token, err)
					}
					rule.Parts = append(rule.Parts, n)
				}
				pos++
			default:
				return FaultRule{}, fmt.Errorf("unexpected positional parameter %q", token)
			}
		}
	}

	if err := ValidateFaultRule(&rule); err != nil {
		return FaultRule{}, err
	}

	return rule, nil
}

// CorruptPayloadSlice returns a newly allocated copy of raw with up to n
// (default 16 if n <= 0) bytes in the middle/start of the slice XORed with 0xFF.
//
// When this corrupted slice is subsequently encoded into a yEnc article, the
// encoder calculates a valid trailer CRC for the corrupted bytes. This simulates
// silent payload bitrot where the CRC check passes but the payload is corrupt.
func CorruptPayloadSlice(raw []byte, n int) []byte {
	if len(raw) == 0 {
		return []byte{}
	}
	if n <= 0 {
		n = 16
	}

	out := slices.Clone(raw)
	k := min(n, len(out))
	start := len(out) / 2
	if start+k > len(out) {
		start = max(0, len(out)-k)
	}
	for i := start; i < start+k; i++ {
		out[i] ^= 0xFF
	}
	return out
}

// CorruptWireBody returns a newly allocated copy of yencBody where up to n
// (default 16 if n <= 0) payload bytes on data lines are mutated without updating
// the yEnc trailer checksum. When decoded with decoder.DecodeArticle, this produces
// decoder.ErrCRCMismatch.
func CorruptWireBody(yencBody []byte, n int) []byte {
	if len(yencBody) == 0 {
		return []byte{}
	}
	if n <= 0 {
		n = 16
	}

	out := slices.Clone(yencBody)

	// Locate start of data lines after =ybegin and optional =ypart.
	ybeginIdx := bytes.Index(out, []byte("=ybegin"))
	if ybeginIdx < 0 {
		return out
	}
	firstNL := bytes.IndexByte(out[ybeginIdx:], '\n')
	if firstNL < 0 {
		return out
	}
	bodyStart := ybeginIdx + firstNL + 1

	if bytes.HasPrefix(out[bodyStart:], []byte("=ypart")) {
		partNL := bytes.IndexByte(out[bodyStart:], '\n')
		if partNL < 0 {
			return out
		}
		bodyStart += partNL + 1
	}

	// Locate start of trailer =yend.
	trailerIdx := bytes.Index(out[bodyStart:], []byte("\n=yend"))
	switch {
	case trailerIdx >= 0:
		trailerIdx += bodyStart + 1
	case bytes.HasPrefix(out[bodyStart:], []byte("=yend")):
		trailerIdx = bodyStart
	default:
		return out
	}

	if bodyStart >= trailerIdx {
		return out
	}

	// Collect candidate byte indices in data lines.
	// Skip escape characters '=' and their escaped payload byte, as well as line endings.
	var candidates []int
	for i := bodyStart; i < trailerIdx; {
		b := out[i]
		if b == '=' {
			i += 2
			continue
		}
		if b == '\r' || b == '\n' {
			i++
			continue
		}
		candidates = append(candidates, i)
		i++
	}

	if len(candidates) == 0 {
		return out
	}

	count := min(n, len(candidates))
	for j := range count {
		idx := candidates[j]
		orig := out[idx]
		mutated := orig ^ 0x01
		for mutated == '=' || mutated == '\r' || mutated == '\n' || mutated == '.' {
			mutated ^= 0x02
		}
		out[idx] = mutated
	}

	return out
}

// RateLimiter controls transmission bandwidth using a time-slot reservation limiter.
type RateLimiter struct {
	mu       sync.Mutex
	rateMBps float64
	nextFree time.Time
}

// NewRateLimiter creates a RateLimiter with the given megabytes-per-second limit (0 for unlimited).
func NewRateLimiter(rateMBps float64) *RateLimiter {
	return &RateLimiter{
		rateMBps: rateMBps,
	}
}

// SetRateMBps updates the rate limit and clears any pending reservation backlog.
func (r *RateLimiter) SetRateMBps(rateMBps float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateMBps = rateMBps
	r.nextFree = time.Time{}
}

// RateMBps returns the current rate limit in MB/s.
func (r *RateLimiter) RateMBps() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rateMBps
}

// Wait blocks until nBytes can be transmitted according to the rate limit,
// or until ctx is canceled.
func (r *RateLimiter) Wait(ctx context.Context, nBytes int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if nBytes <= 0 {
		return nil
	}

	r.mu.Lock()
	rate := r.rateMBps
	if rate <= 0 {
		r.mu.Unlock()
		return nil
	}

	now := time.Now()
	if r.nextFree.Before(now) {
		r.nextFree = now
	}
	durationSeconds := float64(nBytes) / (rate * 1e6)
	transmissionDuration := time.Duration(durationSeconds * float64(time.Second))
	r.nextFree = r.nextFree.Add(transmissionDuration)
	reservedEnd := r.nextFree
	delay := reservedEnd.Sub(now)
	r.mu.Unlock()

	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		r.mu.Lock()
		if rem := time.Until(reservedEnd); rem > 0 {
			refund := min(rem, transmissionDuration)
			r.nextFree = r.nextFree.Add(-refund)
		}
		r.mu.Unlock()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
