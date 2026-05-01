// Package types defines shared data types used across internal packages.
package types

import "github.com/hobeone/gonzbd/internal/constants"

// PPInherit is the sentinel value for FetchOptions.PP meaning
// "inherit from the job's category". This mirrors Python SABnzbd's -1.
const PPInherit = -1

// FetchOptions holds optional parameters for NZB ingest operations
// (via URL grabber, watched folder, or manual upload).
type FetchOptions struct {
	Category string
	Password string
	NzbName  string             // Display name override (Python: nzbname)
	PP       int                // Post-processing level; PPInherit = inherit from category
	Script   string             // Post-processing script name
	Priority constants.Priority // Queue priority; 0 = Normal
}
