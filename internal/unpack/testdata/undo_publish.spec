pkg ./internal/unpack/
run ^TestUndoPublish$

[undoPublish drops the removal errors it collected]
file internal/unpack/snapshot.go
--- anchor
	return errors.Join(errs...)
--- replace
	return nil
--- end

[undoPublish stops at the first removal that fails]
file internal/unpack/snapshot.go
--- anchor
		if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
--- replace
		if err := root.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
--- end
