// Package types defines shared data types used across internal packages.
package types

import "github.com/hobeone/gonzbd/internal/constants"

// PPInherit is the sentinel value for FetchOptions.PP meaning
// "inherit from the job's category". This mirrors Python SABnzbd's -1.
const PPInherit = -1

// Post-processing levels.
const (
	PPNone   = 0 // Download only
	PPVerify = 1 // +repair (verify/repair)
	PPRepair = 1 // (alias)
	PPUnpack = 2 // +unpack
	PPDelete = 3 // +delete
)

// FetchOptions holds optional parameters for NZB ingest operations
// (via URL grabber, watched folder, or manual upload).
type FetchOptions struct {
	JobID    string // Explicit job ID override (e.g. for retrying history jobs)
	Category string
	Password string
	NzbName  string             // Display name override (Python: nzbname)
	PP       int                // Post-processing level; PPInherit = inherit from category
	Script   string             // Post-processing script name
	Priority constants.Priority // Queue priority; 0 = Normal
}
