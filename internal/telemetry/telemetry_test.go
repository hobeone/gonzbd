package telemetry

import "testing"

func TestReset(t *testing.T) {
	ArticlesReceived.Add(10)
	ArticlesWritten.Add(5)
	DiskWrites.Add(100)
	CacheHits.Add(50)
	FilesCompleted.Add(3)
	PipelineErrors.Add("nntp_no_article", 7)

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
	if v := PipelineErrors.Get("nntp_no_article"); v != nil {
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
