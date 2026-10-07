package unpack

// maxAutoDecodeWorkers caps the auto setting: measured gains on compressed
// text flatten at 2-4 workers while CPU cost keeps growing.
const maxAutoDecodeWorkers = 4

// DecodeWorkers resolves the rar_decode_workers setting to the worker count
// passed to rarengine's Reader.SetWorkers. A setting <= 0 (auto, and any
// negative value) yields min(numCPU, 4), at least 1; any other setting is
// returned unchanged (rarengine itself clamps values above 8).
func DecodeWorkers(setting, numCPU int) int {
	if setting > 0 {
		return setting
	}
	return max(1, min(numCPU, maxAutoDecodeWorkers))
}
