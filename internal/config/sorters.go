package config

// SorterConfig is a renaming / sorting rule applied to completed jobs.
// See spec §9.7.
type SorterConfig struct {
	// Name uniquely identifies the sorter within the config.
	Name string `yaml:"name" json:"name"`
	// Order controls evaluation order; lower runs first.
	Order int `yaml:"order" json:"order"`

	// MinSize is the lower job-size threshold for this sorter. 0 means
	// "no minimum". Smaller jobs are passed through unchanged.
	MinSize ByteSize `yaml:"min_size" json:"min_size"`

	// MultipartLabel is appended to disambiguate multi-part series
	// (e.g. "Part 1 of 3"). Optional.
	MultipartLabel string `yaml:"multipart_label" json:"multipart_label"`

	// SortString is the rename template. Tokens (e.g. "%t" for title)
	// are documented in the spec.
	SortString string `yaml:"sort_string" json:"sort_string"`

	// SortCats lists the category names this sorter applies to.
	SortCats []string `yaml:"sort_cats" json:"sort_cats"`

	// SortType is a bitmask of guessit content types this sorter
	// matches: 1=TV, 2=Movie, 4=Date, etc. Stored as a slice for
	// forward compatibility with new types.
	SortType []int `yaml:"sort_type" json:"sort_type"`

	// IsActive enables the sorter; false keeps it in config but skips
	// evaluation.
	IsActive bool `yaml:"is_active" json:"is_active"`
}
