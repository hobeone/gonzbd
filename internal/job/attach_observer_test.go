package job

import "testing"

// The attach observer is a read instant: it runs after a successful
// AttachArticles, once the job is resident, and not for an attach the job
// refused, where nothing became visible to a reader.
func TestAttachObserver_RunsOnlyForASuccessfulAttach(t *testing.T) {
	ft := NewFileTable(twoFileSpecs())
	m, err := NewManifestFromArticles(ft, twoFileArticles())
	if err != nil {
		t.Fatalf("NewManifestFromArticles: %v", err)
	}
	j := New("j", "j", Policy{})
	var calls, residentAtCall int
	j.SetAttachObserver(func() {
		calls++
		if j.Resident() {
			residentAtCall++
		}
	})

	if _, err := j.AttachArticles(m, nil); err == nil {
		t.Fatal("AttachArticles before AttachFiles: nil error")
	}
	if calls != 0 {
		t.Errorf("the observer ran %d times for an attach refused for want of a file table, want 0", calls)
	}

	if err := j.AttachFiles(ft, nil); err != nil {
		t.Fatalf("AttachFiles: %v", err)
	}
	if _, err := j.AttachArticles(m, nil); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
	if calls != 1 || residentAtCall != 1 {
		t.Errorf("after a successful attach the observer ran %d times, %d of them with the job resident, want 1 and 1", calls, residentAtCall)
	}

	if _, err := j.AttachArticles(m, nil); err == nil {
		t.Fatal("AttachArticles on a resident job: nil error")
	}
	if calls != 1 {
		t.Errorf("the observer ran %d times in total after a refused second attach, want 1", calls)
	}
}

// SetAttachObserver(nil) uninstalls the observer, and does not leave a nil
// function for the next attach to call.
func TestSetAttachObserver_NilUninstalls(t *testing.T) {
	j, m := evictedThreeArticleJob(t)
	j.SetAttachObserver(func() { t.Error("an uninstalled observer ran") })
	j.SetAttachObserver(nil)
	if _, err := j.AttachArticles(m, nil); err != nil {
		t.Fatalf("AttachArticles: %v", err)
	}
}
