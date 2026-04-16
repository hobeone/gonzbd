package rss

import (
	"regexp"
	"time"
)

// FilterType controls how a Filter's pattern is applied.
type FilterType int

const (
	// IncludeFilter keeps an item only if its title matches the pattern.
	IncludeFilter FilterType = iota
	// ExcludeFilter drops an item if its title matches the pattern.
	ExcludeFilter
	// RequireFilter is an alias for IncludeFilter with explicit "must have" semantics.
	RequireFilter
	// IgnoreFilter is sugar for ExcludeFilter ("ignore items matching this").
	IgnoreFilter
)

// Filter is one rule in a feed's filter chain.
type Filter struct {
	// Type governs how a match result is interpreted.
	Type FilterType
	// Pattern is a compiled regular expression matched against Item.Title.
	Pattern *regexp.Regexp
	// Name is a human-readable label used in log output.
	Name string
}

// Match reports whether the filter's pattern matches the item title.
// An empty Pattern always returns false.
func (f Filter) Match(item Item) bool {
	if f.Pattern == nil {
		return false
	}
	return f.Pattern.MatchString(item.Title)
}

// Apply evaluates items against the filter chain plus size/age bounds.
//
// Filter evaluation rules (first matching filter wins):
//   - IncludeFilter / RequireFilter: item is kept.
//   - ExcludeFilter / IgnoreFilter: item is dropped.
//
// Items that match no filter are kept when the chain contains only
// exclusion-type filters (or is empty). Items are dropped when the chain
// contains at least one inclusion filter and the item matched none of them.
//
// Size and age checks are applied after filter evaluation.
// minBytes == 0 means no lower bound; maxBytes == 0 means no upper bound.
// maxAge == 0 means no age limit.
func Apply(items []Item, filters []Filter, minBytes, maxBytes int64, maxAge time.Duration) []Item {
	hasInclude := false
	for _, f := range filters {
		if f.Type == IncludeFilter || f.Type == RequireFilter {
			hasInclude = true
			break
		}
	}

	out := items[:0:0]
	for _, item := range items {
		if !passesFilters(item, filters, hasInclude) {
			continue
		}
		if !passesSizeAge(item, minBytes, maxBytes, maxAge) {
			continue
		}
		out = append(out, item)
	}
	return out
}

// passesFilters evaluates the filter chain for one item.
func passesFilters(item Item, filters []Filter, hasInclude bool) bool {
	for _, f := range filters {
		matched := f.Match(item)
		switch f.Type {
		case IncludeFilter, RequireFilter:
			if matched {
				return true
			}
		case ExcludeFilter, IgnoreFilter:
			if matched {
				return false
			}
		}
	}
	// No filter matched: keep when there are no include-type rules.
	return !hasInclude
}

// passesSizeAge applies size and age bounds.
func passesSizeAge(item Item, minBytes, maxBytes int64, maxAge time.Duration) bool {
	if minBytes > 0 && item.Size > 0 && item.Size < minBytes {
		return false
	}
	if maxBytes > 0 && item.Size > 0 && item.Size > maxBytes {
		return false
	}
	if maxAge > 0 {
		age := time.Since(item.Published)
		if age > maxAge {
			return false
		}
	}
	return true
}
