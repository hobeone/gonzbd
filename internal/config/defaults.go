package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/hobeone/gonzbd/internal/constants"
)

// DefaultCleanupList is the list of regex patterns used to strip spam from job names.
var DefaultCleanupList = []string{
	`^(?i)\[PRiVATE\]-?`,
	`^(?i)\[nzbndx\]-?`,
	`^(?i)\[DrunkenSlug\]-?`,
	`^(?i)\[Geek\]-?`,
	`^\[.*\]-?`,
	`^(?i)www\..*\.[a-z]{2,3}-?`,
	`(?i)-? ?\(Scenzbd\)$`,
	`(?i)-? ?\(Obfuscated\)$`,
	`(?i)-? ?\(NZBGeek\)$`,
}

// runningInDockerImage reports whether this binary is the official Docker
// image, via the GONZBD_DOCKER=1 marker set in the Dockerfile's runtime
// stage. The image deliberately does not install bubblewrap: bwrap needs an
// unprivileged user+mount namespace that a normal (non-`--privileged`)
// container's seccomp/AppArmor profile blocks, so installing it would only
// make every extraction silently fail rather than provide working
// containment. Default() uses this to seed StrictSandbox=false for brand-new
// container configs, avoiding a hard abort on missing bwrap; the pipeline
// still enforces post-extraction path containment regardless (see
// internal/postproc/stage_unpack.go). A config file that already sets
// strict_sandbox explicitly is never touched by this — only the seed value
// for a config that doesn't exist yet is affected.
func runningInDockerImage() bool {
	return os.Getenv("GONZBD_DOCKER") == "1"
}

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
			Port:         4289,
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
			BandwidthMax:       0, // unlimited
			BandwidthPerc:      100,
			MinFreeSpace:       ByteSize(1024 * constants.MiB),
			WriteCacheSize:     ByteSize(constants.DefaultWriteCacheBytes),
			CheckpointInterval: int(constants.DefaultCheckpointInterval.Seconds()),
			CheckpointBytes:    ByteSize(constants.DefaultCheckpointBytes),
			MaxArtTries:        3,
			MaxArtOpt:          1,
			MaxActiveJobs:      4,
			TopOnly:            false,
			NoPenalties:        false,
			PreCheck:           false,
			OnDemandPar2:       true,
			PropagationDelay:   0,
			ReplaceIllegalWith: "_",
			ReplaceSpacesWith:  "",
			StripDiacritics:    false,
			CleanupList:        DefaultCleanupList,
		},
		PostProc: PostProcConfig{
			EnableUnrar:             true,
			Enable7zip:              true,
			EnableTar:               true,
			EnableFileJoin:          true,
			EnableTSJoin:            true,
			EnableRecursive:         true,
			EnableParCleanup:        true,
			EnableRarCleanup:        true,
			ProcessUnpackedPar2:     true,
			Par2Command:             "", // auto-detect
			UnrarCommand:            "", // auto-detect
			SevenzCommand:           "", // auto-detect
			Par2Turbo:               false,
			Par2MaxPacketBodySize:   67108864, // 64 MiB
			Par2MaxJunkScanBytes:    65536,    // 64 KiB
			IgnoreUnrarDates:        false,
			OverwriteFiles:          false,
			FlatUnpack:              false,
			DeobfuscateFilenames:    true,
			DirectUnpackThreads:     3, // match SABnzbd default
			StrictSandbox:           !runningInDockerImage(),
			UseGoRAR:                true,
			UseGo7z:                 true,
			UseGoPar2:               true,
			EnableRarVolumeRecovery: true,
			GoRarFallback:           true,
			Go7zFallback:            true,
			GoPar2Fallback:          true,
			EnableQuickCheck:        true,
		},
		Servers:    nil, // user must add at least one before download is possible
		Categories: []CategoryConfig{BuiltinDefaultCategory()},
	}, nil
}

// newAPIKey returns a 16-character lowercase hex string drawn from
// crypto/rand. Caller-facing errors are wrapped with context.
//
// 8 bytes (64 bits) is deliberate, not an oversight: SABnzbd's own
// api_key/nzb_key are 16 lowercase hex chars, enforced here by
// apiKeyPattern (internal/config/validate.go), and third-party clients
// (Sonarr, Radarr, NZB360) expect that format. 64 bits is adequate
// against online brute force (see the "auth" oracle rationale in
// internal/api/router.go), but it is thin for a credential that is
// long-lived, appears in URLs, and is exported to every post-processing
// script as SAB_API_KEY (internal/postproc/script.go) — a compromised
// script is a full API-key compromise. The session key used for the web
// UI is a separate, larger 32-byte value (see server.go). Revisit only
// if the SABnzbd-compat requirement is ever dropped. Accepted risk,
// tracked as issue #112 (S7).
func newAPIKey() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}
