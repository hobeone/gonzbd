pkg ./internal/par2/
run TestIdentification_NothingIdentified

[NothingIdentified ignores identified files]
file internal/par2/identify.go
--- anchor
	return len(id.Files) == 0 && len(id.Unaccounted) > 0
--- replace
	return len(id.Unaccounted) > 0
--- end

# A set with no entries is not the Layout B signature of anything.
[NothingIdentified ignores whether the set names any file]
file internal/par2/identify.go
--- anchor
	return len(id.Files) == 0 && len(id.Unaccounted) > 0
--- replace
	return len(id.Files) == 0
--- end
