pkg ./internal/unpack/
run TestSyncAndPublishJoin_

[syncAndPublishJoin drops the directory open error]
file internal/unpack/filejoin.go
--- anchor
		return fmt.Errorf("filejoin: open output directory: %w", err)
--- replace
		return nil
--- end

[syncAndPublishJoin drops the directory fsync error]
file internal/unpack/filejoin.go
--- anchor
	if err := syncDir(dirFile); err != nil {
--- replace
	if err := syncDir(dirFile); false && err != nil {
--- end
