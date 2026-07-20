package app

import (
	"github.com/hobeone/gonzbd/internal/cmdutil"
	"github.com/hobeone/gonzbd/internal/config"
	"github.com/hobeone/gonzbd/internal/par2"
	"github.com/hobeone/gonzbd/internal/postproc"
	"github.com/hobeone/gonzbd/internal/unpack"
)

// unpackConfigFromPP maps a PostProcConfig snapshot to the UnpackStage's own
// config bundle. This is the single source of truth for the config→unpack
// mapping, used identically at construction (buildStages) and on hot reload
// (ReloadPostProcOptions) so the two paths cannot drift.
//
// probe supplies the construction-only HasProblem flag; extraUnrarArgs is
// pre-parsed by the caller (parsing can fail and is handled there). The
// function itself is pure and cannot fail.
func unpackConfigFromPP(pp config.PostProcConfig, probe binaryProbe, cmdCfg cmdutil.CmdConfig, extraUnrarArgs []string) postproc.UnpackConfig {
	return postproc.UnpackConfig{
		Base: unpack.Options{
			UnrarCommand:     pp.UnrarCommand,
			SevenZipCommand:  pp.SevenzCommand,
			OverwriteFiles:   pp.OverwriteFiles,
			IgnoreUnrarDates: pp.IgnoreUnrarDates,
			OneFolder:        pp.FlatUnpack,
			UseGoRAR:         pp.UseGoRAR,
			GoRarFallback:    pp.GoRarFallback,
			UseGo7z:          pp.UseGo7z,
			Go7zFallback:     pp.Go7zFallback,
			HasProblem:       probe.UnrarInfo.HasProblem,
			CmdCfg:           cmdCfg,
			Sandbox:          cmdutil.SandboxConfig{Enabled: true, Strict: pp.StrictSandbox},
			ExtraArgs:        extraUnrarArgs,
		},
		Permissions:     pp.Permissions,
		PasswordFile:    pp.PasswordFile,
		EnableFileJoin:  pp.EnableFileJoin,
		EnableTar:       pp.EnableTar,
		EnableRecursive: pp.EnableRecursive,
		Cleanup:         pp.EnableRarCleanup,
	}
}

// repairConfigFromPP maps a PostProcConfig snapshot to the RepairStage's own
// config bundle. Single source of truth for the config→repair mapping, used at
// construction and on reload. probe supplies the construction-only par2 Caps
// pointer; extraPar2Args is pre-parsed by the caller. Pure; cannot fail.
//
// ParseOpts is intentionally not mapped here: it has no runtime setter and is
// applied only at construction (see buildStages), so RepairStage.Apply leaves
// it untouched.
func repairConfigFromPP(pp config.PostProcConfig, probe binaryProbe, cmdCfg cmdutil.CmdConfig, extraPar2Args []string) postproc.RepairConfig {
	return postproc.RepairConfig{
		Par2: par2.RunOptions{
			Command:   pp.Par2Command,
			Turbo:     pp.Par2Turbo,
			CmdCfg:    cmdCfg,
			ExtraArgs: extraPar2Args,
			Caps:      &probe.Par2Caps,
		},
		UseGoPar2:      pp.UseGoPar2,
		GoPar2Fallback: pp.GoPar2Fallback,
	}
}
