pkg ./internal/app/
run TestArchiveMemberNames

[inspectNative lists through an extraction Reader]
file internal/rarheader/rarheader.go
--- anchor
		for fh, listErr := range rarengine.ListVolume(f) {
			if fh == nil {
				return listErr
			}
--- replace
		volumesChan := make(chan io.ReadCloser, 1)
		volumesChan <- f
		close(volumesChan)
		r := rarengine.NewReader(volumesChan)
		defer func() { _ = r.Close() }() //nolint:errcheck // mutation
		for {
			entry, nextErr := r.NextEntry()
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				return nextErr
			}
			fh := entry.Header
--- end

