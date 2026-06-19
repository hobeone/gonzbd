package directunpack

import "testing"

func TestAnalyzeRarFilename_Delegates(t *testing.T) {
	set, vol := AnalyzeRarFilename("movie.part02.rar")
	if set != "movie" || vol != 2 {
		t.Errorf("AnalyzeRarFilename = (%q, %d), want (\"movie\", 2)", set, vol)
	}
}
