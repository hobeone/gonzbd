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
	// category. constants.DefaultPriority means "use the global default":
	// the Default/"*" category's priority, then Normal (ResolvePriority).
	Priority int `yaml:"priority" json:"priority"`

	// Dir is a subdirectory within complete_dir to receive output for
	// this category. Empty writes directly to complete_dir.
	Dir string `yaml:"dir" json:"dir"`
}

// ResolvePriority returns the priority a job added under category gets when
// it asked for p. A request for constants.DefaultPriority inherits the
// category's priority; a category priority that is itself not a concrete job
// priority (DefaultPriority, StopPriority, or a value IsValid rejects) defers
// to the fallback category FindCategory returns for "" (Default, then "*"),
// and to NormalPriority when that is not concrete either. Any other p is
// returned unchanged.
//
// It exists so that no sentinel reaches the dispatcher's header from a
// category: dispatch order sorts on Header.Priority, and -100 stored raw sorts
// a job behind Low.
func ResolvePriority(cats []CategoryConfig, category string, p constants.Priority) constants.Priority {
	if p != constants.DefaultPriority {
		return p
	}
	if c, ok := concretePriority(FindCategory(cats, category).Priority); ok {
		return c
	}
	if c, ok := concretePriority(FindCategory(cats, "").Priority); ok {
		return c
	}
	return constants.NormalPriority
}

// concretePriority reports whether a configured category priority names a
// priority a job can hold, rather than a sentinel or an unknown value.
func concretePriority(v int) (constants.Priority, bool) {
	if v < -128 || v > 127 {
		return 0, false
	}
	p := constants.Priority(v) //nolint:gosec // G115: range checked above
	if !p.IsValid() || p == constants.DefaultPriority || p == constants.StopPriority {
		return 0, false
	}
	return p, true
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
