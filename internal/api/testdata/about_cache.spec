pkg ./internal/api/
run TestModeAbout_|TestHostPathModes_RequireFullKey

[the cache check never hits]
file internal/api/about.go
--- anchor
	return sl.val, now.Before(sl.expires)
--- replace
	return sl.val, false
--- end

[the family check is skipped]
file internal/api/about.go
--- anchor
a.Zone() == "" && inFamily(a, wantV6) {
--- replace
a.Zone() == "" {
--- end

[a 4in6 address is accepted as v6]
file internal/api/about.go
--- anchor
return a.Is6() && !a.Is4In6()
--- replace
return a.Is6()
--- end

[a zoned address is accepted]
file internal/api/about.go
--- anchor
a.Zone() == "" && inFamily(a, wantV6) {
--- replace
inFamily(a, wantV6) {
--- end

[a failed lookup is cached for the full TTL]
file internal/api/about.go
--- anchor
	ttl := publicIPNegativeTTL
--- replace
	ttl := publicIPTTL
--- end

[the fetch is tied to the caller's context]
file internal/api/about.go
--- anchor
	raw := fetch(context.WithoutCancel(ctx), urlStr)
--- replace
	raw := fetch(ctx, urlStr)
--- end

[about accepts the NZB key again]
file internal/api/router.go
--- anchor
"about":       {handler: s.modeAbout, level: LevelAdmin},
--- replace
"about":       {handler: s.modeAbout, level: LevelProtected},
--- end

[status_overview accepts the NZB key again]
file internal/api/router.go
--- anchor
"status_overview": {handler: s.modeStatusOverview, level: LevelAdmin},
--- replace
"status_overview": {handler: s.modeStatusOverview, level: LevelProtected},
--- end

[concurrent callers each fetch]
file internal/api/about.go
--- anchor
	ch := slot.flight.DoChan("", func() (any, error) {
--- replace
	ch := slot.flight.DoChan(time.Now().String(), func() (any, error) {
--- end
