pkg ./internal/app/
run Test(HandleArticleWritten_RecordsOnTheWritingInstance|HandleArticleRejected_FailsOnTheWritingInstance|HandleFileUntrusted_LeavesADepartedInstancesRowsAlone|CompleteFinalizedFile_RefusesADepartedInstance)$

# Each per-job assembler callback names the instance it reports on, and each handler
# acts on that instance (#1056). Each mutation here puts the by-ID lookup back
# in one handler, so a report for an attempt that has left the queue lands on
# the retry registered under its ID; the two registered guards are neutered
# separately, since each protects ID-keyed state the departed instance must
# not touch.

[handleArticleWritten records on whatever holds the ID]
file internal/app/record.go
--- anchor
func (app *Application) handleArticleWritten(owner assembler.Owner, fileIdx int, artIdx int32, off, n int64, crc uint32) {
	j := instanceOf(owner)
--- replace
func (app *Application) handleArticleWritten(owner assembler.Owner, fileIdx int, artIdx int32, off, n int64, crc uint32) {
	j, _ := app.dispatcher.Job(owner.ID())
--- end

[handleArticleRejected fails the article on whatever holds the ID]
file internal/app/durability.go
--- anchor
func (app *Application) handleArticleRejected(owner assembler.Owner, fileIdx int, artIdx int32, reason string, uncovered int64) {
	j := instanceOf(owner)
--- replace
func (app *Application) handleArticleRejected(owner assembler.Owner, fileIdx int, artIdx int32, reason string, uncovered int64) {
	j, _ := app.dispatcher.Job(owner.ID())
--- end

[handleFileUntrusted untrusts whatever holds the ID]
file internal/app/record.go
--- anchor
func (app *Application) handleFileUntrusted(owner assembler.Owner, fileIdx int) {
	j := instanceOf(owner)
--- replace
func (app *Application) handleFileUntrusted(owner assembler.Owner, fileIdx int) {
	j, _ := app.dispatcher.Job(owner.ID())
--- end

[handleFileUntrusted acts for a departed instance]
file internal/app/record.go
--- anchor
	if !app.registered(j) {
		app.log.Debug("untrusted file not recorded; the instance has left the queue",
--- replace
	if false {
		app.log.Debug("untrusted file not recorded; the instance has left the queue",
--- end

[completeFinalizedFile marks whatever holds the ID]
file internal/app/app.go
--- anchor
	j := fc.Job
	if !app.registered(j) {
--- replace
	j, _ := app.dispatcher.Job(fc.Job.ID())
	if !app.registered(j) {
--- end

[completeFinalizedFile marks a departed instance's file]
file internal/app/app.go
--- anchor
	if !app.registered(j) {
		err := job.ErrNotResident
--- replace
	if false {
		err := job.ErrNotResident
--- end
