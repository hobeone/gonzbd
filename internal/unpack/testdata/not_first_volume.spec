pkg ./internal/unpack/
run TestGoUnRAR_MiddleVolumeNamedAsFirstFails

[not-first-volume classification neutered]
file internal/unpack/go_unrar.go
--- anchor
	case IsNotFirstVolume(err):
		return FailMissingVolume
--- replace
	case false:
		return FailMissingVolume
--- end

[not-first-volume output line neutered]
file internal/unpack/go_unrar.go
--- anchor
				if IsNotFirstVolume(err) {
					what = "not the first volume of its set"
--- replace
				if false {
					what = "not the first volume of its set"
--- end
