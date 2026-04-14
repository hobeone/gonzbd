package config

// ScheduleConfig is a single cron-like scheduled action. See spec §9.8 and
// §14 for the supported action vocabulary.
type ScheduleConfig struct {
	// Name uniquely identifies the schedule within the config.
	Name string `yaml:"name"`
	// Enabled toggles execution without removing the entry.
	Enabled bool `yaml:"enabled"`

	// Action is the action vocabulary keyword (e.g. "speedlimit",
	// "pause", "resume", "shutdown"). See spec §14.1.
	Action string `yaml:"action"`
	// Arguments is a free-form parameter string passed to the action.
	Arguments string `yaml:"arguments"`

	// Minute is the minute spec ("*", "0-59", or comma list).
	Minute string `yaml:"minute"`
	// Hour is the hour spec ("*", "0-23", or comma list).
	Hour string `yaml:"hour"`
	// DayOfWeek is the day-of-week spec ("*", "1-7" with 1=Monday, or
	// comma list).
	DayOfWeek string `yaml:"dayofweek"`
}
