package config

import "sync"

// Config is the deserialized form of sabnzbd.yaml. It is the single source
// of truth for runtime tuning parameters; all other packages receive a
// reference to it via constructor injection rather than reading a global.
//
// The mutex protects all fields. Callers that need to read several related
// fields atomically should call With/WithRead for a brief snapshot rather
// than holding the lock across long operations.
type Config struct {
	mu sync.RWMutex

	General    GeneralConfig    `yaml:"general" json:"general"`
	Downloads  DownloadConfig   `yaml:"downloads" json:"downloads"`
	PostProc   PostProcConfig   `yaml:"postproc" json:"postproc"`
	Servers    []ServerConfig   `yaml:"servers" json:"servers"`
	Categories []CategoryConfig `yaml:"categories" json:"categories"`
	Sorters    []SorterConfig   `yaml:"sorters,omitempty" json:"sorters,omitempty"`
	Schedules  []ScheduleConfig `yaml:"schedules,omitempty" json:"schedules,omitempty"`
	RSS        []RSSFeedConfig  `yaml:"rss,omitempty" json:"rss,omitempty"`
}

// WithRead invokes fn with a read lock held. The Config pointer passed to
// fn must not be retained or mutated. It exists so callers can read several
// related fields without races against concurrent Save/Reload.
//
// WARNING: The callback MUST NOT call any other Config methods that acquire
// the lock (such as Set) as this will cause an immediate deadlock.
func (c *Config) WithRead(fn func(*Config)) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	fn(c)
}

// With invokes fn with a write lock held. fn may freely mutate any
// embedded fields. After fn returns, the caller is responsible for
// triggering any change-notification callbacks (the callback subsystem is
// added when the first subscriber appears; see the package doc).
//
// WARNING: The callback MUST NOT call any other Config methods that acquire
// the lock (such as Set or WithRead) as this will cause an immediate deadlock.
// If you need to mutate configuration via reflection, use SetLocked instead.
func (c *Config) With(fn func(*Config)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}
