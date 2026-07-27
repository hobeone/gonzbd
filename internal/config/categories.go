package config

import (
	"strings"

	"github.com/hobeone/gonzbd/internal/constants"
)

// BuiltinDefaultCategory returns the hardcoded baseline category used
// when no Default / "*" / explicit match is found in the loaded config.
//
// It exists so that broken or partial configs never silently disable
// post-processing: returning a zero CategoryConfig{} would set PP=0
// (download only) and skip Repair/Unpack/Delete, which is almost never
// what the user intends. PP=3 (+ Delete sources) matches the value
// seeded by Default() when a brand-new config is created.
//
// Treat the return as immutable; callers must not mutate the returned
// struct since it is by-value and any mutation would only affect the
// caller's copy.
func BuiltinDefaultCategory() CategoryConfig {
	return CategoryConfig{
		Name:     "Default",
		PP:       3, // + Delete sources
		Script:   "None",
		Priority: int(constants.NormalPriority),
	}
}

// CategoryConfig is a per-category override bundle. Jobs added under a
// category inherit that category's defaults for priority, post-processing
// flags, and destination subdirectory. See spec §9.5.
type CategoryConfig struct {
	// Name is the unique category identifier. The reserved names
	// "Default" and "*" select fallback / catch-all behavior; both must
	// always exist in a valid configuration.
	Name string `yaml:"name" json:"name"`

	// PP is the post-processing level (cumulative, not a bitmask):
	//   0 = Download only
	//   1 = + Repair (par2 verify/repair)
	//   2 = + Unpack (implies repair)
	//   3 = + Delete sources (implies unpack + repair)
	// Values > 3 are treated as 3 for compatibility with SABnzbd configs.
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
// is empty or not found, it falls back to "Default", then "*", and
// finally to BuiltinDefaultCategory() if neither fallback exists. Name
// matching is case-insensitive: a job tagged "tv" matches a config
// entry named "TV", and the "Default" / "*" fallbacks match regardless
// of casing in the YAML.
//
// FindCategory never returns a zero-value CategoryConfig — callers can
// rely on the returned PP/Script/Priority being meaningful values.
func FindCategory(cats []CategoryConfig, name string) CategoryConfig {
	var defaultCat, starCat CategoryConfig
	var foundDefault, foundStar bool
	for _, c := range cats {
		if name != "" && strings.EqualFold(c.Name, name) {
			return c
		}
		if strings.EqualFold(c.Name, "Default") {
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
	return BuiltinDefaultCategory()
}
