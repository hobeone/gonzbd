package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// binaryProbe holds the results of probing external tool binaries at startup.
// Accepted by buildStages so tests can inject fake results without spawning
// real processes.
type binaryProbe struct {
	Par2Caps   par2.Par2Caps
	UnrarInfo  unpack.UnrarInfo
	SevenzInfo unpack.SevenzInfo
}

// probeBinaries probes the three external tool binaries used by the
// post-processing pipeline and logs what was found. Separated from
// buildStages so tests can skip the probe by constructing a binaryProbe
// directly. Also fixes the accidental double-detection of unrar that
// previously occurred (once in New() and once in buildStages).
func probeBinaries(ctx context.Context, cfg Config, log *slog.Logger) binaryProbe {
	ppLog := log.With("component", "postproc")

	par2Caps := par2.DetectCapabilities(ctx, cfg.Par2Command)
	if par2Caps.IsTurbo && !cfg.Par2Turbo {
		ppLog.Info("Detected par2cmdline-turbo; consider enabling par2_turbo for faster repair")
	}
	if par2Caps.Version != "" {
		ppLog.Info("Detected par2 binary", "version", par2Caps.Version)
	}

	unrarInfo := unpack.DetectUnrar(ctx, cfg.UnrarCommand)
	if unrarInfo.Available {
		ppLog.Info("Detected unrar binary", "version", unrarInfo.VersionStr)
		if unrarInfo.HasProblem {
			ppLog.Warn("unrar binary version < 5.50; some flags will be disabled")
		}
	} else {
		ppLog.Warn("unrar binary not found; RAR extraction will not be available")
	}

	sevenzInfo := unpack.DetectSevenZip(ctx, cfg.SevenzCommand)
	if sevenzInfo.Available {
		ppLog.Info("Detected 7z binary", "version", sevenzInfo.Version)
	} else {
		ppLog.Warn("7z binary not found; 7-Zip extraction will not be available")
	}

	return binaryProbe{
		Par2Caps:   par2Caps,
		UnrarInfo:  unrarInfo,
		SevenzInfo: sevenzInfo,
	}
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
func buildStages(cfg Config, log *slog.Logger, probe binaryProbe) (builtStages, error) {
	ppLog := log.With("component", "postproc")
	var stages []postproc.Stage

	// Quick-check stage: relocate flat files into par2-expected subdirs
	// and CRC-verify assembled files before repair runs. The stage is
	// always in the pipeline so it can be toggled at runtime via
	// Application.SetQuickCheckEnabled without restarting.
	qcStage := postproc.NewQuickCheckStage()
	qcStage.Log = ppLog
	qcStage.SetEnabled(!cfg.SkipQuickCheck)
	stages = append(stages, qcStage)

	// Build nice/ionice wrapping config for all external tool commands.
	cmdCfg := cmdutil.CmdConfig{Nice: cfg.Nice, Ionice: cfg.Ionice}

	// Parse user-supplied extra params (validated: must start with '-').
	extraPar2Args, err := cmdutil.ParseExtraParams(cfg.ExtraPar2Params)
	if err != nil {
		return builtStages{}, fmt.Errorf("extra_par2_params: %w", err)
	}
	extraUnrarArgs, err := cmdutil.ParseExtraParams(cfg.ExtraUnrarParams)
	if err != nil {
		return builtStages{}, fmt.Errorf("extra_unrar_params: %w", err)
	}
	// N5: Validate extra unrar params against SABnzbd's allowlist.
	// Warn instead of hard-fail so existing configs aren't broken.
	if err := cmdutil.ValidateUnrarParams(extraUnrarArgs); err != nil {
		ppLog.Warn("extra_unrar_params contains non-standard flags",
			"err", err,
			"hint", "SABnzbd only allows: -mlp, -om*, -ri*")
	}

	// Repair stage.
	repairStage := postproc.NewRepairStageWith(
		par2.RunOptions{
			Command:   cfg.Par2Command,
			Turbo:     cfg.Par2Turbo,
			CmdCfg:    cmdCfg,
			ExtraArgs: extraPar2Args,
			Caps:      &probe.Par2Caps,
		},
	)
	repairStage.Log = ppLog
	repairStage.UseGoPar2 = cfg.UseGoPar2
	repairStage.GoPar2Fallback = cfg.GoPar2Fallback
	stages = append(stages, repairStage)

	// Unpack stage: always included in pipeline, enabled dynamically.
	unpackStage := postproc.NewUnpackStageWith(unpack.Options{
		UnrarCommand:     cfg.UnrarCommand,
		SevenZipCommand:  cfg.SevenzCommand,
		OverwriteFiles:   cfg.OverwriteFiles,
		IgnoreUnrarDates: cfg.IgnoreUnrarDates,
		OneFolder:        cfg.FlatUnpack,
		UseGoRAR:         cfg.UseGoRAR,
		GoRarFallback:    cfg.GoRarFallback,
		UseGo7z:          cfg.UseGo7z,
		Go7zFallback:     cfg.Go7zFallback,
		HasProblem:       probe.UnrarInfo.HasProblem,
		CmdCfg:           cmdCfg,
		ExtraArgs:        extraUnrarArgs,
	}, cfg.EnableRarCleanup)
	unpackStage.Permissions = cfg.Permissions
	unpackStage.PasswordFile = cfg.PasswordFile
	unpackStage.EnableFileJoin = cfg.EnableFileJoin
	unpackStage.EnableRecursive = cfg.EnableRecursive
	unpackStage.Log = ppLog
	unpackStage.SetEnabled(cfg.EnableUnrar || cfg.Enable7zip || cfg.EnableFileJoin)
	stages = append(stages, unpackStage)

	// Sample cleanup runs after unpack so it sees both raw and extracted files.
	sampleStage := postproc.NewSampleCleanupStage()
	sampleStage.Log = ppLog
	sampleStage.SetEnabled(cfg.IgnoreSamples)
	stages = append(stages, sampleStage)

	// Par2-based filename recovery: unconditional, runs after unpack.
	// Must run before par2_cleanup since it reads the .par2 files.
	par2RenameStage := postproc.NewRecoverPar2NamesStage()
	par2RenameStage.Log = ppLog
	stages = append(stages, par2RenameStage)

	// Par2 cleanup: delete .par2 files after everything that needs them
	// has run (repair, unpack, par2 rename). Gated on both repair and
	// unpack success so par2 files survive for debugging when things fail.
	par2CleanupStage := postproc.NewPar2CleanupStage(cfg.EnableParCleanup)
	par2CleanupStage.Log = ppLog
	stages = append(stages, par2CleanupStage)

	// Deobfuscation stage.
	deobStage := postproc.NewDeobfuscateStage()
	deobStage.Log = ppLog
	deobStage.SetEnabled(cfg.DeobfuscateFilenames)
	stages = append(stages, deobStage)

	// Extension cleanup: delete files matching the user's cleanup list.
	cleanupStage := postproc.NewExtensionCleanupStage(cfg.CleanupExtensions)
	cleanupStage.Log = ppLog
	stages = append(stages, cleanupStage)

	// Finalize: move files from download dir to complete dir.
	// Must run BEFORE script so scripts receive the final directory path.
	finalizeStage := postproc.NewFinalizeStage()
	finalizeStage.Log = ppLog
	finalizeStage.SetFolderRename(cfg.FolderRename)
	stages = append(stages, finalizeStage)

	// Cleanup: remove admin sidecar data (__ADMIN__ dir) from the job dir.
	cleanupAdminStage := postproc.NewCleanupStage()
	cleanupAdminStage.Log = ppLog
	stages = append(stages, cleanupAdminStage)

	// Script stage: runs AFTER finalize so job.DownloadDir points to the
	// final complete_dir, matching SABnzbd's $1 convention.
	scriptStage := postproc.NewScriptStage(
		cfg.ScriptDir, cfg.CompleteDir,
		cfg.Version, cfg.APIKey, cfg.ListenAddr,
	)
	scriptStage.Log = ppLog
	scriptStage.SetScriptCanFail(cfg.ScriptCanFail)
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
