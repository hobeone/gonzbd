pkg ./internal/fsutil/
run TestOpenNoFollow$

# OpenNoFollow opens one path component inside a root with O_NOFOLLOW, so a
# symlink in the name's place is refused wherever it points, and a name that
# is not one component never reaches openat. The opened inode must be a
# regular file with one link, so a hard link to a sibling is refused too.

[O_NOFOLLOW dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
			flag|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, uint32(perm.Perm()))
--- replace
			flag|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, uint32(perm.Perm()))
--- end

[the one-component check dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
--- replace
	if false && strings.ContainsRune(name, filepath.Separator) {
--- end

[the link-count check dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	if st.Nlink != 1 {
--- replace
	if false {
--- end

[the regular-file check dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
--- replace
	if false {
--- end

[the O_TRUNC refusal dropped]
file internal/fsutil/nofollow_unix.go
--- anchor
	if flag&os.O_TRUNC != 0 {
--- replace
	if false {
--- end

[the descriptor left non-blocking]
file internal/fsutil/nofollow_unix.go
--- anchor
	if err := unix.SetNonblock(fd, false); err != nil {
--- replace
	if err := unix.SetNonblock(fd, true); err != nil {
--- end
