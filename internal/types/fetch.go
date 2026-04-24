package types

// FetchOptions holds optional parameters for NZB ingest operations
// (via URL grabber, watched folder, or manual upload).
type FetchOptions struct {
	Category string
	Password string
}
