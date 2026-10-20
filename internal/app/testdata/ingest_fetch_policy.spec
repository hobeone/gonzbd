pkg ./internal/app/
run TestSeedJobFiles_WritesTheStructure

# BuildIngestJob takes its fetch policy from the FileTable, the one classifier
# of recovery volumes. With the read neutered no file is held, so every file
# stays FetchAlways.

[ingest never holds a recovery volume]
file internal/app/ingest.go
--- anchor
		if onDemandPar2 && j.Files().IsPar2Recovery(fi) {
--- replace
		if onDemandPar2 && j.Files().IsPar2Recovery(fi) && false {
--- end
