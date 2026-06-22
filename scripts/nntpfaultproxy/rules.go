package main

import (
	"slices"

	"math/rand/v2"
)

// matchRule returns the first rule that applies to messageID, or false if
// no rule matches. Rules with a non-empty MessageIDs list match only those
// exact IDs; rules with Rate > 0 (and an empty MessageIDs list) match that
// fraction of all other requests, decided by rng. Pass a seeded *rand.Rand
// for deterministic tests; the CLI seeds one from -seed or the current
// time for real runs.
func matchRule(rules []Rule, messageID string, rng *rand.Rand) (Rule, bool) {
	for _, r := range rules {
		if len(r.MessageIDs) > 0 {
			if slices.Contains(r.MessageIDs, messageID) {
				return r, true
			}
			continue
		}
		if r.Rate > 0 && rng.Float64() < r.Rate {
			return r, true
		}
	}
	return Rule{}, false
}
