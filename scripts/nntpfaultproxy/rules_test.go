package main

import (
	"math/rand/v2"
	"testing"
)

func TestMatchRule_ExactMessageID(t *testing.T) {
	rules := []Rule{
		{MessageIDs: []string{"a@x", "b@x"}, Action: "drop"},
	}
	rng := rand.New(rand.NewPCG(1, 1))

	got, ok := matchRule(rules, "b@x", rng)
	if !ok || got.Action != "drop" {
		t.Fatalf("matchRule(b@x) = %+v, %v; want drop, true", got, ok)
	}

	_, ok = matchRule(rules, "c@x", rng)
	if ok {
		t.Fatal("matchRule(c@x) matched, want no match")
	}
}

func TestMatchRule_RateAlwaysMatches(t *testing.T) {
	rules := []Rule{{Rate: 1.0, Action: "corrupt"}}
	rng := rand.New(rand.NewPCG(1, 1))
	for i := range 20 {
		got, ok := matchRule(rules, "any@x", rng)
		if !ok || got.Action != "corrupt" {
			t.Fatalf("iteration %d: matchRule = %+v, %v; want corrupt, true", i, got, ok)
		}
	}
}

func TestMatchRule_RateNeverMatches(t *testing.T) {
	rules := []Rule{{Rate: 0, Action: "corrupt"}}
	rng := rand.New(rand.NewPCG(1, 1))
	for i := range 20 {
		if _, ok := matchRule(rules, "any@x", rng); ok {
			t.Fatalf("iteration %d: matched with Rate=0, want no match", i)
		}
	}
}

func TestMatchRule_FirstMatchWins(t *testing.T) {
	rules := []Rule{
		{MessageIDs: []string{"a@x"}, Action: "drop"},
		{MessageIDs: []string{"a@x"}, Action: "corrupt"},
	}
	rng := rand.New(rand.NewPCG(1, 1))
	got, ok := matchRule(rules, "a@x", rng)
	if !ok || got.Action != "drop" {
		t.Fatalf("matchRule = %+v, %v; want first rule (drop), true", got, ok)
	}
}

func TestMatchRule_NoRulesNoMatch(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	if _, ok := matchRule(nil, "any@x", rng); ok {
		t.Fatal("matchRule with no rules matched, want no match")
	}
}
