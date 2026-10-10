package assembler

// The control-message FileIdx values. A negative FileIdx is the marker that a
// WriteRequest carries an operation for the worker rather than an article, and
// each value names which operation.
//
// They are declared together because that convention is load-bearing: it is
// the reason WriteArticle keeps the identity fields on WriteRequest rather than
// moving them onto ArticleRef outright — control messages share the channel and
// set those same fields. Each is an operation the worker performs on its own
// goroutine, because it owns every file handle (X1): the caller puts a request
// on the worker's channel and blocks on ackCh until the worker has answered.
const (
	// fileIdxCancelJob asks the worker to drop a job's in-flight work and
	// close its handles.
	fileIdxCancelJob = -1

	// fileIdxCloseHandles asks the worker to close a job's handles, leaving
	// the job itself alone.
	fileIdxCloseHandles = -2

	// fileIdxForgetJob asks the worker to drop a job's tombstones so its
	// files can be written again. A retry reuses the job ID, and the
	// tombstone set is keyed on (jobID, fileIdx) and never expires
	// otherwise — see Assembler.ForgetJob.
	fileIdxForgetJob = -4

	// fileIdxQuiesce asks the worker to answer once everything queued ahead
	// of it has been processed — see Assembler.Quiesce.
	fileIdxQuiesce = -5
)
