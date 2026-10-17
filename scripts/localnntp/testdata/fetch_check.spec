pkg ./scripts/localnntp/
run TestVerify_Complete_SingleFetchAcrossServers|TestCLI_Verify_FetchCheckFlags|TestSumServedClean

[an ID served more than once is not a failure]
file scripts/localnntp/verify.go
--- anchor
	if len(report.MultiServedIDs) > 0 {
--- replace
	if false {
--- end

[nothing served clean passes vacuously]
file scripts/localnntp/verify.go
--- anchor
	if sum == 0 {
--- replace
	if false {
--- end

[a non-par2 segment that was never served is not a failure]
file scripts/localnntp/verify.go
--- anchor
	if missing := unservedSegments(m, totals); len(missing) > 0 {
--- replace
	if missing := unservedSegments(m, totals); false && len(missing) > 0 {
--- end

[par2 segments are required to be served]
file scripts/localnntp/verify.go
--- anchor
		if f.Role == RolePar2Index || f.Role == RolePar2Volume {
--- replace
		if false {
--- end

[a duplicated fetch-check URL is accepted]
file scripts/localnntp/verify.go
--- anchor
		if seenURLs[norm] {
--- replace
		if false {
--- end

[runVerify drops --expect-single-fetch]
file scripts/localnntp/main.go
--- anchor
		ExpectSingleFetch:  *expectSingleFetch,
--- replace
		ExpectSingleFetch:  *expectSingleFetch && false,
--- end

[runVerify drops --servers-for-fetch-check]
file scripts/localnntp/main.go
--- anchor
		FetchCheckURLs:     fetchCheckURLs,
--- replace
		FetchCheckURLs:     nil,
--- end
