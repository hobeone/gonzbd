package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"

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

// probeBinaries probes the three external tool binaries used by the
// post-processing pipeline and logs what was found. Separated from
// buildStages so tests can skip the probe by constructing a binaryProbe
// directly. Also fixes the accidental double-detection of unrar that
// previously occurred (once in New() and once in buildStages).
func probeBinaries(ctx context.Context, cfg *config.Config, log *slog.Logger) binaryProbe {
	ppLog := log.With("component", "postproc")

	var par2Command, unrarCommand, sevenzCommand string
	var par2Turbo bool
	cfg.WithRead(func(c *config.Config) {
		par2Command = c.PostProc.Par2Command
		par2Turbo = c.PostProc.Par2Turbo
		unrarCommand = c.PostProc.UnrarCommand
		sevenzCommand = c.PostProc.SevenzCommand
	})

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
func buildStages(cfg *config.Config, version string, log *slog.Logger, probe binaryProbe) (builtStages, error) {
	ppLog := log.With("component", "postproc")
	var stages []postproc.Stage

	var enableQuickCheck, par2Turbo, useGoPar2, goPar2Fallback, enableRarCleanup bool
	var enableFileJoin, enableTar, enableRecursive, enableUnrar, enable7zip, ignoreSamples bool
	var enableParCleanup, deobfuscateFilenames, folderRename, scriptCanFail bool
	var par2Command, unrarCommand, sevenzCommand, permissions, passwordFile string
	var scriptDir, completeDir, apiKey, listenAddr string
	var nice, ionice, extraPar2Params, extraUnrarParams string
	var cleanupExtensions []string
	var flatUnpack, overwriteFiles, ignoreUnrarDates, useGoRAR, goRarFallback, useGo7z, go7zFallback, strictSandbox bool

	cfg.WithRead(func(c *config.Config) {
		enableQuickCheck = c.PostProc.EnableQuickCheck
		par2Command = c.PostProc.Par2Command
		par2Turbo = c.PostProc.Par2Turbo
		useGoPar2 = c.PostProc.UseGoPar2
		goPar2Fallback = c.PostProc.GoPar2Fallback
		enableRarCleanup = c.PostProc.EnableRarCleanup
		enableFileJoin = c.PostProc.EnableFileJoin
		enableTar = c.PostProc.EnableTar
		enableRecursive = c.PostProc.EnableRecursive
		enableUnrar = c.PostProc.EnableUnrar
		enable7zip = c.PostProc.Enable7zip
		ignoreSamples = c.PostProc.IgnoreSamples
		enableParCleanup = c.PostProc.EnableParCleanup
		deobfuscateFilenames = c.PostProc.DeobfuscateFilenames
		folderRename = c.PostProc.FolderRename
		scriptCanFail = c.PostProc.ScriptCanFail
		unrarCommand = c.PostProc.UnrarCommand
		sevenzCommand = c.PostProc.SevenzCommand
		permissions = c.PostProc.Permissions
		passwordFile = c.PostProc.PasswordFile
		scriptDir = c.General.ScriptDir
		completeDir = c.General.CompleteDir
		nice = c.PostProc.Nice
		ionice = c.PostProc.Ionice
		extraPar2Params = c.PostProc.ExtraPar2Params
		extraUnrarParams = c.PostProc.ExtraUnrarParams
		cleanupExtensions = c.PostProc.CleanupExtensions
		flatUnpack = c.PostProc.FlatUnpack
		overwriteFiles = c.PostProc.OverwriteFiles
		ignoreUnrarDates = c.PostProc.IgnoreUnrarDates
		useGoRAR = c.PostProc.UseGoRAR
		goRarFallback = c.PostProc.GoRarFallback
		useGo7z = c.PostProc.UseGo7z
		go7zFallback = c.PostProc.Go7zFallback
		strictSandbox = c.PostProc.StrictSandbox

		apiKey = c.General.APIKey
		listenAddr = net.JoinHostPort(c.General.Host, strconv.Itoa(c.General.Port))
	})

	// Quick-check stage: relocate flat files into par2-expected subdirs
	// and CRC-verify assembled files before repair runs. The stage is
	// always in the pipeline so it can be toggled at runtime via
	// Application.SetQuickCheckEnabled without restarting.
	qcStage := postproc.NewQuickCheckStage()
	qcStage.Log = ppLog
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
			Command:   par2Command,
			Turbo:     par2Turbo,
			CmdCfg:    cmdCfg,
			ExtraArgs: extraPar2Args,
			Caps:      &probe.Par2Caps,
		},
	)
	repairStage.Log = ppLog
	repairStage.UseGoPar2 = useGoPar2
	repairStage.GoPar2Fallback = goPar2Fallback
	stages = append(stages, repairStage)

	// Unpack stage: always included in pipeline, enabled dynamically.
	unpackStage := postproc.NewUnpackStageWith(unpack.Options{
		UnrarCommand:     unrarCommand,
		SevenZipCommand:  sevenzCommand,
		OverwriteFiles:   overwriteFiles,
		IgnoreUnrarDates: ignoreUnrarDates,
		OneFolder:        flatUnpack,
		UseGoRAR:         useGoRAR,
		GoRarFallback:    goRarFallback,
		UseGo7z:          useGo7z,
		Go7zFallback:     go7zFallback,
		HasProblem:       probe.UnrarInfo.HasProblem,
		CmdCfg:           cmdCfg,
		Sandbox: cmdutil.SandboxConfig{
			Enabled: true,
			Strict:  strictSandbox,
		},
		ExtraArgs: extraUnrarArgs,
	}, enableRarCleanup)
	unpackStage.Permissions = permissions
	unpackStage.PasswordFile = passwordFile
	unpackStage.EnableFileJoin = enableFileJoin
	unpackStage.EnableTar = enableTar
	unpackStage.EnableRecursive = enableRecursive
	unpackStage.Log = ppLog
	unpackStage.SetEnabled(enableUnrar || enable7zip || enableFileJoin || enableTar)
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
