package config

import (
	"fmt"
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
	// the Default/"*" category's priority, then Normal; PausedPriority
	// pauses those jobs, which take the global default (ResolvePriority).
	Priority int `yaml:"priority" json:"priority"`

	// Dir is a subdirectory within complete_dir to receive output for
	// this category. Empty writes directly to complete_dir.
	Dir string `yaml:"dir" json:"dir"`
}

// ResolvePriority turns the priority a job under category was asked for into
// the priority it holds, and reports whether the request also pauses it.
// Ingest (BuildIngestJob) and the API's name=priority and change_cat
// (Server.applyPriority) share it:
// `git grep -n 'config\.ResolvePriority(' -- '*.go' ':!*_test.go'` returns 2 lines.
//
//   - A concrete p (constants.Priority.IsConcrete) is returned unchanged.
//   - constants.DefaultPriority inherits the category's priority. A category
//     priority of Paused pauses the job and keeps looking; one that is
//     Repair, Default, Stop or undefined is skipped. The search runs over
//     the category, then the fallback FindCategory returns for "" (Default,
//     then "*"), and ends at NormalPriority.
//   - constants.PausedPriority pauses the job, which holds what the same
//     search finds.
//
// Any other p — Stop or an undefined value — is an error, so no caller can
// store it: dispatch order sorts on Header.Priority, and a sentinel stored
// raw sorts a job behind Low, an undefined value anywhere at all.
func ResolvePriority(cats []CategoryConfig, category string, p constants.Priority) (constants.Priority, bool, error) {
	pause := p == constants.PausedPriority
	if p != constants.DefaultPriority && !pause {
		if !p.IsConcrete() {
			return 0, false, fmt.Errorf("priority %d is not a priority a job can be given", p)
		}
		return p, false, nil
	}
	for _, name := range []string{category, ""} {
		c, ok := categoryPriority(FindCategory(cats, name).Priority)
		if !ok {
			continue
		}
		if c == constants.PausedPriority {
			pause = true
			continue
		}
		return c, pause, nil
	}
	return constants.NormalPriority, pause, nil
}

// categoryPriority reports whether a configured category priority is Force,
// High, Normal, Low or Paused. Repair, which configuration may not give a
// job, Default, Stop and unknown values are not.
func categoryPriority(v int) (constants.Priority, bool) {
	if v < -128 || v > 127 {
		return 0, false
	}
	p := constants.Priority(v) //nolint:gosec // G115: range checked above
	if p == constants.RepairPriority {
		return 0, false
	}
	if !p.IsConcrete() && p != constants.PausedPriority {
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
