package config

import (
	"maps"
	"slices"
	"sync"
)

// Config is the deserialized form of gonzbd.yaml. It is the single source
// of truth for runtime tuning parameters; all other packages receive a
// reference to it via constructor injection rather than reading a global.
//
// The mutex protects all fields. Callers that need to read several related
// fields atomically should call value getters (such as GetGeneral(), GetDownloads(),
// or IngestSnapshot()) rather than holding the lock across long operations.
type Config struct {
	mu sync.RWMutex

	General       GeneralConfig      `yaml:"general" json:"general"`
	Downloads     DownloadConfig     `yaml:"downloads" json:"downloads"`
	PostProc      PostProcConfig     `yaml:"postproc" json:"postproc"`
	Servers       []ServerConfig     `yaml:"servers" json:"servers"`
	Categories    []CategoryConfig   `yaml:"categories" json:"categories"`
	Notifications NotificationConfig `yaml:"notifications,omitempty" json:"notifications"`
}

// GetGeneral returns a value-copy snapshot of the GeneralConfig section,
// deep-cloning all reference fields to prevent external mutation.
func (c *Config) GetGeneral() GeneralConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	gen := c.General
	gen.LocalRanges = slices.Clone(c.General.LocalRanges)
	gen.LogLevels = maps.Clone(c.General.LogLevels)
	return gen
}

// GetDownloads returns a value-copy snapshot of the DownloadConfig section,
// deep-cloning all reference fields to prevent external mutation.
func (c *Config) GetDownloads() DownloadConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	dl := c.Downloads
	dl.CleanupList = slices.Clone(c.Downloads.CleanupList)
	return dl
}

// GetPostProc returns a value-copy snapshot of the PostProcConfig section,
// deep-cloning all reference fields to prevent external mutation.
func (c *Config) GetPostProc() PostProcConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	pp := c.PostProc
	pp.CleanupExtensions = slices.Clone(c.PostProc.CleanupExtensions)
	return pp
}

// GetServers returns a deep copy of the ServerConfig slice.
func (c *Config) GetServers() []ServerConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Servers)
}

// GetCategories returns a deep copy of the CategoryConfig slice.
func (c *Config) GetCategories() []CategoryConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return slices.Clone(c.Categories)
}

// GetNotifications returns a value-copy snapshot of the NotificationConfig section,
// deep-cloning all internal slice reference fields to prevent external mutation.
func (c *Config) GetNotifications() NotificationConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := c.Notifications
	n.Email.To = slices.Clone(c.Notifications.Email.To)
	n.Email.Events = slices.Clone(c.Notifications.Email.Events)
	n.Apprise.Events = slices.Clone(c.Notifications.Apprise.Events)
	n.Script.Events = slices.Clone(c.Notifications.Script.Events)
	return n
}

// IngestSnapshot bundles Downloads and Categories into a single atomic snapshot
// for NZB file and URL ingestion.
type IngestSnapshot struct {
	Downloads  DownloadConfig
	Categories []CategoryConfig
}

// IngestSnapshot returns an atomic snapshot of Downloads and Categories.
func (c *Config) IngestSnapshot() IngestSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	dl := c.Downloads
	dl.CleanupList = slices.Clone(c.Downloads.CleanupList)
	return IngestSnapshot{
		Downloads:  dl,
		Categories: slices.Clone(c.Categories),
	}
}

// Snapshot returns a pointer to a new Config snapshot across all 6 top-level
// configuration sections under a single read lock, deep-cloning all reference fields.
func (c *Config) Snapshot() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	res := &Config{
		General:       c.General,
		Downloads:     c.Downloads,
		PostProc:      c.PostProc,
		Servers:       slices.Clone(c.Servers),
		Categories:    slices.Clone(c.Categories),
		Notifications: c.Notifications,
	}
	res.General.LocalRanges = slices.Clone(c.General.LocalRanges)
	res.General.LogLevels = maps.Clone(c.General.LogLevels)
	res.Downloads.CleanupList = slices.Clone(c.Downloads.CleanupList)
	res.PostProc.CleanupExtensions = slices.Clone(c.PostProc.CleanupExtensions)
	res.Notifications.Email.To = slices.Clone(c.Notifications.Email.To)
	res.Notifications.Email.Events = slices.Clone(c.Notifications.Email.Events)
	res.Notifications.Apprise.Events = slices.Clone(c.Notifications.Apprise.Events)
	res.Notifications.Script.Events = slices.Clone(c.Notifications.Script.Events)
	return res
}

// With invokes fn with a write lock held. fn may freely mutate any
// embedded fields. After fn returns, the caller is responsible for
// triggering any change-notification callbacks (the callback subsystem is
// added when the first subscriber appears; see the package doc).
//
// WARNING: The callback MUST NOT call any other Config methods that acquire
// the lock (such as Set) as this will cause an immediate deadlock.
// If you need to mutate configuration via reflection, use SetLocked instead.
func (c *Config) With(fn func(*Config)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}
