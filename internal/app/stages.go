package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// binaryProbe holds the results of probing external tool binaries at startup.
// Accepted by buildStages so tests can inject fake results without spawning
// real processes.
type binaryProbe struct {
	Par2Caps   par2.Caps
	UnrarInfo  unpack.UnrarInfo
	SevenzInfo unpack.SevenzInfo
}

var (
	probeCacheMu sync.RWMutex
	probeCache   = make(map[string]binaryProbe)
)

// probeBinaries probes the three external tool binaries used by the
// post-processing pipeline and logs what was found. Separated from
// buildStages so tests can skip the probe by constructing a binaryProbe
// directly. Also fixes the accidental double-detection of unrar that
// previously occurred (once in New() and once in buildStages).
func probeBinaries(ctx context.Context, cfg *config.Config, log *slog.Logger) binaryProbe {
	pp := cfg.GetPostProc()
	par2Command := pp.Par2Command
	par2Turbo := pp.Par2Turbo
	unrarCommand := pp.UnrarCommand
	sevenzCommand := pp.SevenzCommand

	cacheKey := fmt.Sprintf("%s|%t|%s|%s", par2Command, par2Turbo, unrarCommand, sevenzCommand)
	probeCacheMu.RLock()
	cached, ok := probeCache[cacheKey]
	probeCacheMu.RUnlock()
	if ok {
		return cached
	}

	ppLog := log.With("component", "postproc")

	par2Caps := par2.DetectCapabilities(ctx, par2Command)
	if par2Caps.IsTurbo && !par2Turbo {
		ppLog.Info("Detected par2cmdline-turbo; consider enabling par2_turbo for faster repair")
	}
	if par2Caps.Version != "" {
		ppLog.Info("Detected par2 binary", "version", par2Caps.Version)
	}

	unrarInfo := unpack.DetectUnrar(ctx, unrarCommand)
	if unrarInfo.Available {
		ppLog.Info("Detected unrar binary", "version", unrarInfo.VersionStr)
		if unrarInfo.HasProblem {
			ppLog.Warn("unrar binary version < 5.50; some flags will be disabled")
		}
	} else {
		ppLog.Warn("unrar binary not found; RAR extraction will not be available")
	}

	sevenzInfo := unpack.DetectSevenZip(ctx, sevenzCommand)
	if sevenzInfo.Available {
		ppLog.Info("Detected 7z binary", "version", sevenzInfo.Version)
	} else {
		ppLog.Warn("7z binary not found; 7-Zip extraction will not be available")
	}

	probe := binaryProbe{
		Par2Caps:   par2Caps,
		UnrarInfo:  unrarInfo,
		SevenzInfo: sevenzInfo,
	}
	probeCacheMu.Lock()
	probeCache[cacheKey] = probe
	probeCacheMu.Unlock()
	return probe
}

// builtStages bundles the stage list with the individually addressable stages
// that need runtime toggling via Application setter methods.
type builtStages struct {
	Stages           []postproc.Stage
	QuickCheck       *postproc.QuickCheckStage
	Repair           *postproc.RepairStage
	Par2Cleanup      *postproc.Par2CleanupStage
	Unpack           *postproc.UnpackStage
	Finalize         *postproc.FinalizeStage
	Script           *postproc.ScriptStage
	SampleCleanup    *postproc.SampleCleanupStage
	Deobfuscate      *postproc.DeobfuscateStage
	ExtensionCleanup *postproc.ExtensionCleanupStage
}

// buildStages constructs the post-processing stage list from cfg and a
// pre-computed binaryProbe. Called once during New() when customStages is nil.
// Separated so the stage-wiring block doesn't live inside the New() constructor.
// The returned builtStages holds pointers to individually-addressable stages
// that can be toggled at runtime without restart.
func buildStages(cfg *config.Config, version string, log *slog.Logger, probe binaryProbe) (builtStages, error) {
	ppLog := log.With("component", "postproc")
	var stages []postproc.Stage

	// pp is a whole-struct snapshot used by the shared config→stage translators
	// (unpackConfigFromPP / repairConfigFromPP). The remaining locals feed the
	// one-liner stages and construction-only settings.
	snap := cfg.Snapshot()
	pp := snap.PostProc
	gen := &snap.General
	enableQuickCheck := pp.EnableQuickCheck
	enableRarVolumeRecovery := pp.EnableRarVolumeRecovery
	ignoreSamples := pp.IgnoreSamples
	enableParCleanup := pp.EnableParCleanup
	deobfuscateFilenames := pp.DeobfuscateFilenames
	folderRename := pp.FolderRename
	scriptCanFail := pp.ScriptCanFail
	nice := pp.Nice
	ionice := pp.Ionice
	extraPar2Params := pp.ExtraPar2Params
	extraUnrarParams := pp.ExtraUnrarParams
	cleanupExtensions := pp.CleanupExtensions
	scriptDir := gen.ScriptDir
	completeDir := gen.CompleteDir

	par2ParseOpts := par2.ParseOptionsFromConfig(&pp)

	apiKey := gen.APIKey
	listenAddr := net.JoinHostPort(gen.Host, strconv.Itoa(gen.Port))

	// Quick-check stage: relocate flat files into par2-expected subdirs
	// and CRC-verify assembled files before repair runs. The stage is
	// always in the pipeline so it can be toggled at runtime via
	// Application.SetQuickCheckEnabled without restarting.
	qcStage := postproc.NewQuickCheckStage()
	qcStage.Log = ppLog
	qcStage.ParseOpts = par2ParseOpts
	qcStage.SetEnabled(enableQuickCheck)
	stages = append(stages, qcStage)

	// Build nice/ionice wrapping config for all external tool commands.
	cmdCfg := cmdutil.CmdConfig{Nice: nice, Ionice: ionice}

	// Parse user-supplied extra params (validated: must start with '-').
	extraPar2Args, err := cmdutil.ParseExtraParams(extraPar2Params)
	if err != nil {
		return builtStages{}, fmt.Errorf("extra_par2_params: %w", err)
	}
	extraUnrarArgs, err := cmdutil.ParseExtraParams(extraUnrarParams)
	if err != nil {
		return builtStages{}, fmt.Errorf("extra_unrar_params: %w", err)
	}
	// Sanity assert: extra unrar params must conform to SABnzbd's allowlist
	// (enforced at config load and mutation boundaries via Validate()).
	if err := cmdutil.ValidateUnrarParams(extraUnrarArgs); err != nil {
		return builtStages{}, fmt.Errorf("extra_unrar_params: %w", err)
	}

	// Repair stage. Config-driven options come from the shared translator
	// (repairConfigFromPP), the same mapping used on hot reload, so the two
	// paths cannot diverge. ParseOpts is construction-only (no runtime setter).
	repairStage := postproc.NewRepairStage()
	repairStage.Log = ppLog
	repairStage.ParseOpts = par2ParseOpts
	repairStage.Apply(repairConfigFromPP(pp, probe, cmdCfg, extraPar2Args))
	stages = append(stages, repairStage)

	// RAR volume recovery: no-op unless normal filename-based RAR detection
	// found nothing at all. Must run after Repair (so PAR2-based rename
	// recovery, if a PAR2 set exists, gets first chance) and before Unpack
	// (so a successful rename here is visible to Unpack's own Scan()).
	rarVolRecoveryStage := postproc.NewRarVolumeRecoveryStage()
	rarVolRecoveryStage.Log = ppLog
	rarVolRecoveryStage.SetEnabled(enableRarVolumeRecovery)
	stages = append(stages, rarVolRecoveryStage)

	// Unpack stage: always included in pipeline, enabled dynamically. Config
	// via the shared translator (unpackConfigFromPP), the same mapping used on
	// hot reload.
	unpackStage := postproc.NewUnpackStage()
	unpackStage.Log = ppLog
	unpackStage.Apply(unpackConfigFromPP(pp, probe, cmdCfg, extraUnrarArgs))
	unpackStage.SetEnabled(pp.EnableUnrar || pp.Enable7zip || pp.EnableFileJoin || pp.EnableTar)
	stages = append(stages, unpackStage)

	// Sample cleanup runs after unpack so it sees both raw and extracted files.
	sampleStage := postproc.NewSampleCleanupStage()
	sampleStage.Log = ppLog
	sampleStage.SetEnabled(ignoreSamples)
	stages = append(stages, sampleStage)

	// Par2-based filename recovery: unconditional, runs after unpack.
	// Must run before par2_cleanup since it reads the .par2 files.
	par2RenameStage := postproc.NewRecoverPar2NamesStage()
	par2RenameStage.Log = ppLog
	stages = append(stages, par2RenameStage)

	// Par2 cleanup: delete .par2 files after everything that needs them
	// has run (repair, unpack, par2 rename). Gated on both repair and
	// unpack success so par2 files survive for debugging when things fail.
	par2CleanupStage := postproc.NewPar2CleanupStage(enableParCleanup)
	par2CleanupStage.Log = ppLog
	par2CleanupStage.ParseOpts = par2ParseOpts
	stages = append(stages, par2CleanupStage)

	// Deobfuscation stage.
	deobStage := postproc.NewDeobfuscateStage()
	deobStage.Log = ppLog
	deobStage.SetEnabled(deobfuscateFilenames)
	stages = append(stages, deobStage)

	// Extension cleanup: delete files matching the user's cleanup list.
	cleanupStage := postproc.NewExtensionCleanupStage(cleanupExtensions)
	cleanupStage.Log = ppLog
	stages = append(stages, cleanupStage)

	// Finalize: move files from download dir to complete dir.
	// Must run BEFORE script so scripts receive the final directory path.
	finalizeStage := postproc.NewFinalizeStage()
	finalizeStage.Log = ppLog
	finalizeStage.SetFolderRename(folderRename)
	stages = append(stages, finalizeStage)

	// Cleanup: remove admin sidecar data (__ADMIN__ dir) from the job dir.
	cleanupAdminStage := postproc.NewCleanupStage()
	cleanupAdminStage.Log = ppLog
	stages = append(stages, cleanupAdminStage)

	// Script stage: runs AFTER finalize so job.DownloadDir points to the
	// final complete_dir, matching SABnzbd's $1 convention.
	scriptStage := postproc.NewScriptStage(
		scriptDir, completeDir,
		version, apiKey, listenAddr,
	)
	scriptStage.Log = ppLog
	scriptStage.SetScriptCanFail(scriptCanFail)
	scriptStage.SetRedactSecrets(cfg.PostProc.RedactScriptSecrets)
	stages = append(stages, scriptStage)

	return builtStages{
		Stages:           stages,
		QuickCheck:       qcStage,
		Repair:           repairStage,
		Par2Cleanup:      par2CleanupStage,
		Unpack:           unpackStage,
		Finalize:         finalizeStage,
		Script:           scriptStage,
		SampleCleanup:    sampleStage,
		Deobfuscate:      deobStage,
		ExtensionCleanup: cleanupStage,
	}, nil
}
