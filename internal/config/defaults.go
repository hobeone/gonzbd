package config

import (
	"crypto/rand"
	"encoding/hex"
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
// The error result is retained for callers and for future failure modes, but
// nothing in this function can currently produce one. It used to carry a
// failure to draw random bytes; since Go 1.24, crypto/rand.Read never returns
// an error to its caller — it calls runtime fatal instead (go.dev/issue/66821)
// — so those two branches were unreachable code that no test could ever
// exercise, and they were removed rather than annotated as exempt.
func Default() (*Config, error) {
	apiKey := newAPIKey()
	nzbKey := newAPIKey()

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
// crypto/rand.
//
// It cannot fail, and so returns no error. crypto/rand.Read has not returned
// one to its caller since Go 1.24: on a failure to gather entropy it calls
// runtime fatal and aborts the process, because a program that silently
// continued with predictable "random" bytes is worse than one that stops
// (go.dev/issue/66821). An error result here would be a branch nothing can
// reach and no test can cover.
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
func newAPIKey() string {
	var buf [8]byte
	// The error is discarded rather than checked: see the doc above. Read
	// either fills buf or takes the process down.
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
