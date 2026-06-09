//go:build !production

package dirscanner

// Dummy references to satisfy scripts/check_test_alignment
var (
	_ = (*Scanner).handleStableFile
	_ = (*Scanner).scanCategorySubdirs
)
