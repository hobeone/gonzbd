package assembler

import "testing"

// TestJobTombstone_DropsTheJobsFileTombstones pins that the cancel and
// close-handles arms leave no per-file tombstone behind for the job whose
// handles they close.
//
// After either arm, a later article for the job is refused at openTargetFile
// by the resolver, before processRequest reads completed, so the job's
// entries are never consulted again. A job handed to post-processing passes
// through the close-handles arm (enqueuePostProc), and only a retry's
// ForgetJob clears what it leaves, so an entry kept here is one per such file
// for the life of the process.
//
// The finished file (fileIdx 0) is the case that grew: it left open when it
// completed, so neither arm's loop over open reaches it. The open file
// (fileIdx 1) is the one the arms used to tombstone themselves. The other
// job's entry must survive: that job is still downloading, and its tombstone
// is what stops a late duplicate reopening its finished file.
func TestJobTombstone_DropsTheJobsFileTombstones(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fileIdx int
	}{
		{"cancel", fileIdxCancelJob},
		{"close-handles", fileIdxCloseHandles},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newHelperAssembler()
			a.opts.OnArticlesUnwritten = func(string, int, []int32) {}

			finished := fileKey{jobID: "job", fileIdx: 0}
			writing := fileKey{jobID: "job", fileIdx: 1}
			bystander := fileKey{jobID: "other", fileIdx: 0}
			open := map[fileKey]*openFile{
				writing: newHelperFile(t, t.TempDir(), "job_1.dat", 1),
			}
			completed := map[fileKey]struct{}{finished: {}, bystander: {}}

			ack := make(chan error, 1)
			a.dispatchRequest(
				WriteRequest{JobID: "", FileIdx: tc.fileIdx, MessageID: "job", ackCh: ack},
				open, completed)

			for _, k := range []fileKey{finished, writing} {
				if _, still := completed[k]; still {
					t.Errorf("completed still holds %+v after the job was tombstoned; "+
						"nothing reads it again and only a retry would remove it", k)
				}
			}
			if _, ok := completed[bystander]; !ok {
				t.Error("another job's tombstone was dropped, so a late duplicate for " +
					"its finished file would reopen it")
			}
		})
	}
}
