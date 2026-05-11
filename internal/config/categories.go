package config

// CategoryConfig is a per-category override bundle. Jobs added under a
// category inherit that category's defaults for priority, post-processing
// flags, and destination subdirectory. See spec §9.5.
type CategoryConfig struct {
	// Name is the unique category identifier. The reserved names
	// "Default" and "*" select fallback / catch-all behavior; both must
	// always exist in a valid configuration.
	Name string `yaml:"name" json:"name"`

	// PP is the post-processing flag bitmask (see spec §8.2):
	//   bit 0 (1) = Repair only
	//   bit 1 (2) = Repair + Unpack
	//   bit 2 (4) = Repair + Unpack + Delete
	// Sentinel value 7 means "all of the above"; Python uses raw ints
	// here, kept as int for round-trip compatibility with external tools.
	PP int `yaml:"pp" json:"pp"`

	// Script is the user post-processing script name to run. The literal
	// string "None" (case-insensitive) suppresses script execution.
	Script string `yaml:"script" json:"script"`

	// Priority is the default priority for jobs added under this
	// category. constants.DefaultPriority means "use the global default".
	Priority int `yaml:"priority" json:"priority"`

	// Dir is a subdirectory within complete_dir to receive output for
	// this category. Empty writes directly to complete_dir.
	Dir string `yaml:"dir" json:"dir"`
}

// FindCategory looks up the CategoryConfig for the given name. If name
// is empty or not found, it falls back to "Default", then "*". Returns
// a zero CategoryConfig if no categories are defined at all.
func FindCategory(cats []CategoryConfig, name string) CategoryConfig {
	var defaultCat, starCat CategoryConfig
	var foundDefault, foundStar bool
	for _, c := range cats {
		if c.Name == name && name != "" {
			return c
		}
		if c.Name == "Default" {
			defaultCat = c
			foundDefault = true
		}
		if c.Name == "*" {
			starCat = c
			foundStar = true
		}
	}
	if foundDefault {
		return defaultCat
	}
	if foundStar {
		return starCat
	}
	return CategoryConfig{}
}
