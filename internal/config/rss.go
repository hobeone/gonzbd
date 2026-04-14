package config

// RSSFeedConfig describes one RSS / Atom feed and the filters applied to
// its items. See spec §9.9 and §12.
type RSSFeedConfig struct {
	// Name uniquely identifies the feed within the config.
	Name string `yaml:"name"`
	// URI is the feed URL.
	URI string `yaml:"uri"`

	// Cat is the category assigned to items added from this feed.
	Cat string `yaml:"cat"`
	// PP is the post-processing flag override; empty inherits the
	// category default.
	PP string `yaml:"pp"`
	// Script is the post-processing script override; empty inherits.
	Script string `yaml:"script"`
	// Enable toggles polling without removing the entry.
	Enable bool `yaml:"enable"`
	// Priority is the queue priority assigned to items added from this
	// feed. constants.DefaultPriority means "use category default".
	Priority int `yaml:"priority"`

	// Filters is the ordered list of filter rules evaluated for each
	// item. The first matching rule wins; an item with no matching rule
	// is rejected.
	Filters []RSSFilterConfig `yaml:"filters,omitempty"`
}

// RSSFilterConfig is one rule within an RSS feed's filter chain.
type RSSFilterConfig struct {
	// Name is a label for the filter (used in logs).
	Name string `yaml:"name"`
	// Enabled toggles evaluation.
	Enabled bool `yaml:"enabled"`

	// Title is a regex matched against the item title.
	Title string `yaml:"title"`
	// Body is a regex matched against the item body / description.
	Body string `yaml:"body"`

	// Cat / PP / Script / Priority override the per-feed defaults when
	// this rule matches. Empty / sentinel values mean "inherit".
	Cat      string `yaml:"cat"`
	PP       string `yaml:"pp"`
	Script   string `yaml:"script"`
	Priority int    `yaml:"priority"`

	// Type controls how Title / Body match results combine:
	//   "require"        — the item must match
	//   "must_not_match" — the item must not match
	//   "ignore"         — the item is ignored if it matches
	Type string `yaml:"type"`

	// SizeFrom / SizeTo bound the accepted item byte size. Either side
	// may be 0 / empty for "no limit".
	SizeFrom ByteSize `yaml:"size_from"`
	SizeTo   ByteSize `yaml:"size_to"`

	// Age is the maximum item age in days from publication. 0 disables.
	Age int `yaml:"age"`
}
