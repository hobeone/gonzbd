package telemetry

import "testing"

func TestReset(t *testing.T) {
	ArticlesReceived.Add(10)
	ArticlesWritten.Add(5)
	DiskWrites.Add(100)
	CacheHits.Add(50)
	FilesCompleted.Add(3)
	PipelineErrors.Add(ErrClassNNTPNoArticle, 7)

	Reset()

	if v := ArticlesReceived.Value(); v != 0 {
		t.Errorf("ArticlesReceived = %d after Reset, want 0", v)
	}
	if v := ArticlesWritten.Value(); v != 0 {
		t.Errorf("ArticlesWritten = %d after Reset, want 0", v)
	}
	if v := DiskWrites.Value(); v != 0 {
		t.Errorf("DiskWrites = %d after Reset, want 0", v)
	}
	if v := CacheHits.Value(); v != 0 {
		t.Errorf("CacheHits = %d after Reset, want 0", v)
	}
	if v := FilesCompleted.Value(); v != 0 {
		t.Errorf("FilesCompleted = %d after Reset, want 0", v)
	}
	if v := PipelineErrors.Get(ErrClassNNTPNoArticle); v != nil {
		t.Errorf("PipelineErrors[nntp_no_article] = %v after Reset, want key absent", v)
	}
}

func TestCountersIncrement(t *testing.T) {
	Reset()

	ArticlesReceived.Add(1)
	ArticlesReceived.Add(1)
	ArticlesReceived.Add(1)

	if v := ArticlesReceived.Value(); v != 3 {
		t.Errorf("ArticlesReceived = %d, want 3", v)
	}

	DiskWriteBytes.Add(1024)
	DiskWriteBytes.Add(2048)

	if v := DiskWriteBytes.Value(); v != 3072 {
		t.Errorf("DiskWriteBytes = %d, want 3072", v)
	}
}

func TestErrorCount(t *testing.T) {
	Reset()
	t.Cleanup(Reset)

	if got := ErrorCount(ErrClassCRCMismatch); got != 0 {
		t.Errorf("ErrorCount(%q) = %d for an absent key, want 0", ErrClassCRCMismatch, got)
	}

	PipelineErrors.Add(ErrClassCRCMismatch, 1)
	PipelineErrors.Add(ErrClassCRCMismatch, 1)

	if got := ErrorCount(ErrClassCRCMismatch); got != 2 {
		t.Errorf("ErrorCount(%q) = %d, want 2", ErrClassCRCMismatch, got)
	}
}
