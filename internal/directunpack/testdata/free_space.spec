pkg ./internal/directunpack/
run ^(TestDirectUnpack_RefusesASetThatDoesNotFit|TestDirectUnpack_ExtractsASetThatFits|TestSettleLocked_ClassifiesDiskFull|TestLeadingVolumes)$

# DirectUnpack applies post-processing's free-space bound to a set, over the
# volumes on disk when volume 1 is, and classifies a set refused for space or
# one that filled the disk as disk full.

[DirectUnpack does not check free space]
file internal/directunpack/directunpack.go
--- anchor
			d.extractDir, d.opts.MinFreeSpace); err != nil {
--- replace
			d.extractDir, d.opts.MinFreeSpace); err != nil && false {
--- end

[a disk-full set is not classified]
file internal/directunpack/directunpack.go
--- anchor
		if unpack.IsDiskFull(err) {
--- replace
		if false {
--- end

[the bound reads volumes past a gap]
file internal/directunpack/directunpack.go
--- anchor
		p, ok := d.completedVols[setname][vol]
		if !ok {
--- replace
		p, ok := d.completedVols[setname][vol]
		if !ok && vol > 8 {
--- end
