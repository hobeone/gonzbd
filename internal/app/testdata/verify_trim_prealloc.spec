pkg ./internal/app/
run ^(TestVerifyJobFiles_KeepsARepairedFilePastAShortLastRow|TestVerifyJobFiles_Outcomes|TestFinishFileByPath)$

# A file finished by path is truncated to its last row's end only while it
# is at its preallocated size (#829). Each arm breaks one half: a file at
# another size (par2's repaired copy) must keep its bytes, and a file at its
# preallocated size must still lose the zeros past its last article.

[a file at any size is truncated]
file internal/app/verify.go
--- anchor
	if st.Size() == prealloc {
--- replace
	if st.Size() >= 0 {
--- end

[a file at its preallocated size is not truncated]
file internal/app/verify.go
--- anchor
	if st.Size() == prealloc {
--- replace
	if st.Size() < 0 {
--- end

[the finish is not told the preallocated size]
file internal/app/verify.go
--- anchor
	if err := finishFileByPath(loc, maxEnd, preallocSize(ft, f.FileIndex)); err != nil {
--- replace
	if err := finishFileByPath(loc, maxEnd, 0); err != nil {
--- end
