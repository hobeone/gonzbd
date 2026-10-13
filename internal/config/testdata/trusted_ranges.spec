pkg ./internal/config/
run ^TestGeneralConfig_TrustedRanges$

[a parsed list is dropped]
file internal/config/trust.go
--- anchor
		return nil
	}
	return ranges
}
--- replace
		return nil
	}
	return ranges[:0]
}
--- end

[an unparsable list trusts its first entry]
file internal/config/trust.go
--- anchor
	ranges, err := ParseLocalRanges(g.LocalRanges)
	if err != nil {
		return nil
--- replace
	ranges, err := ParseLocalRanges(g.LocalRanges)
	if err != nil {
		ranges, _ = ParseLocalRanges(g.LocalRanges[:1])
		return ranges
--- end
