pkg ./internal/fsutil/
run TestCopyAndRemove_RefusesSymlink|TestCopyAndRemove_RefusesDirectory|TestCopyAndRemove_CopyErrorRemovesPartialDst

[the cross-device fallback copies whatever it is given]
file internal/fsutil/move.go
--- anchor
	if !info.Mode().IsRegular() {
--- replace
	if false {
--- end

[the fallback refuses only directories, so a symlink is followed and copied]
file internal/fsutil/move.go
--- anchor
	if !info.Mode().IsRegular() {
--- replace
	if info.IsDir() {
--- end

[a copy error leaves the partial dst behind]
file internal/fsutil/move.go
--- anchor
		return errors.Join(err, in.Close(), out.Close(), os.Remove(dst))
--- replace
		return errors.Join(err, in.Close(), out.Close())
--- end
