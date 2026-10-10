pkg ./internal/app/
run TestUnpackConfigFromPP_ForwardsProbeUnrarVersion

# The detected unrar version must reach the unpack stage, which passes -ol-
# (create no symlinks) only from the unrar version that has the switch.

[the unrar version is not forwarded]
file internal/app/reload_translate.go
--- anchor
			UnrarVersion:     probe.UnrarInfo.Version,
--- replace
			UnrarVersion:     0,
--- end
