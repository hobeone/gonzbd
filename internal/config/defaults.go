package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/hobeone/sabnzbd-go/internal/constants"
)

// Default returns a fully populated Config suitable for first-run use.
// Generated secrets (api_key, nzb_key) are filled with cryptographically
// random 16-character hex strings. Callers that intend to persist the
// returned config should Save it immediately so the same secrets appear
// on subsequent loads.
//
// Default returns an error only when the OS cannot supply random bytes,
// which is treated as fatal — without an api_key the daemon cannot
// authenticate API requests and there is no safe fallback.
func Default() (*Config, error) {
	apiKey, err := newAPIKey()
	if err != nil {
		return nil, fmt.Errorf("default config: generate api_key: %w", err)
	}
	nzbKey, err := newAPIKey()
	if err != nil {
		return nil, fmt.Errorf("default config: generate nzb_key: %w", err)
	}

	return &Config{
		General: GeneralConfig{
			Host:         "127.0.0.1",
			Port:         8080,
			HTTPSPort:    0,
			APIKey:       apiKey,
			NZBKey:       nzbKey,
			DownloadDir:  "Downloads/incomplete",
			CompleteDir:  "Downloads/complete",
			DirscanSpeed: int(constants.DefaultDirScanRate.Seconds()),
			AdminDir:     constants.AdminDirName,
			LogLevel:     "info",
			Language:     "en",
		},
		Downloads: DownloadConfig{
			BandwidthMax:        0, // unlimited
			BandwidthPerc:       100,
			MinFreeSpace:        ByteSize(1024 * constants.MiB),
			MinFreeSpaceCleanup: ByteSize(2048 * constants.MiB),
			ArticleCacheSize:    ByteSize(constants.DefaultArticleCacheBytes),
			MaxArtTries:         3,
			MaxArtOpt:           1,
			TopOnly:             false,
			NoPenalties:         false,
			PreCheck:            false,
			PropagationDelay:    0,
		},
		PostProc: PostProcConfig{
			EnableUnrar:      true,
			Enable7zip:       true,
			EnableParCleanup: true,
			Par2Command:      "par2",
			UnrarCommand:     "", // auto-detect
			SevenzCommand:    "", // auto-detect
			Par2Turbo:        false,
			IgnoreUnrarDates: false,
			OverwriteFiles:   false,
			FlatUnpack:       false,
		},
		Servers: nil, // user must add at least one before download is possible
		Categories: []CategoryConfig{
			{
				Name:     "Default",
				PP:       3, // Repair + Unpack
				Script:   "None",
				Priority: int(constants.NormalPriority),
				Order:    0,
			},
			{
				Name:     "*",
				PP:       3,
				Script:   "None",
				Priority: int(constants.NormalPriority),
				Order:    1,
			},
		},
		Sorters:   nil,
		Schedules: nil,
		RSS:       nil,
	}, nil
}

// newAPIKey returns a 16-character lowercase hex string drawn from
// crypto/rand. Caller-facing errors are wrapped with context.
func newAPIKey() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
