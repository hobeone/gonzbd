pkg ./internal/par2/
run TestIdentify|TestQuickCheck

# Identify is a pure function over a directory and a par2 manifest, so every
# one of its properties is expressible as a mutation. These five cover the
# claims the design rests on; a test suite that survives any of them is
# asserting something weaker than it reads.

# The property that makes extending identification to flat sets safe at all.
# The matchers this replaces relocated a file as an inseparable part of
# matching it, so a flat set would have self-moved every correctly-named file.
# If NeedsRename can be forced true without a test dying, nothing pins that.
[every identification claims it needs a rename]
file internal/par2/identify.go
--- anchor
	return filepath.ToSlash(i.Desc.FileName) != filepath.ToSlash(i.OnDisk)
--- replace
	return true
--- end

# The mirror. Forcing it false must kill the obfuscated and subdirectory
# tests, which are the two cases where a rename is the entire point.
[no identification ever needs a rename]
file internal/par2/identify.go
--- anchor
	return filepath.ToSlash(i.Desc.FileName) != filepath.ToSlash(i.OnDisk)
--- replace
	return false
--- end

# Content matching is what resolves obfuscation. Neutered at the guard rather
# than at the lookup: hashIndex is still built above, so the mutated tree
# compiles and the tests get a real answer instead of COMPILE_ERROR.
[content matching never runs]
file internal/par2/identify.go
--- anchor
	if len(hashIndex) > 0 {
--- replace
	if false {
--- end

# A set describing two files with identical first 16 KB cannot be resolved by
# content, and guessing hands par2 the wrong name. Poisoning the index with -1
# is what refuses; letting the second entry win is the plausible-looking bug.
[an ambiguous hash silently picks the second entry]
file internal/par2/identify.go
--- anchor
			hashIndex[fd.Hash16k] = -1
--- replace
			hashIndex[fd.Hash16k] = ei
--- end

# par2 cannot describe its own files, and a sidecar must never be claimed in
# place of a protected file. Without this the .nfo fixture — deliberately
# given the SAME bytes as the real file — becomes a candidate.
#
# Emptied at the DATA rather than at isIgnoredForIdentification's return:
# replacing that return drops the last use of "strings" and the mutated tree
# fails to build, which says nothing about whether the test discriminates.
# That is not hypothetical — it is what the first draft of this mutation did.
[sidecars become identification candidates]
file internal/par2/identify.go
--- anchor
var ignoredExtensions = []string{".par2", ".sfv", ".nfo"}
--- replace
var ignoredExtensions = []string{}
--- end

# Flattened-name matching survived the move out of matchByFlattenedName. Made
# a duplicate of the basename candidate rather than removed, so "strings" keeps
# its other use in isIgnoredForIdentification and the tree still builds.
[flattened-name matching never fires]
file internal/par2/identify.go
--- anchor
			{strings.ReplaceAll(slashed, "/", "_"), MatchFlattenedName},
--- replace
			{filepath.Base(slashed), MatchFlattenedName},
--- end

# Pass 3 exists for entries sharing a Hash16k. Inverted rather than disabled:
# only CRC-less entries enter the index, so it is empty for any fixture that
# has IFSC data, while fd and ei both stay used and the tree builds.
[whole-file CRC32 matching never fires]
file internal/par2/identify.go
--- anchor
		if claimedEntry[ei] || fd.FileCRC32 == 0 || fd.FileSize == 0 {
--- replace
		if claimedEntry[ei] || fd.FileCRC32 != 0 || fd.FileSize == 0 {
--- end

# The guard that makes extending identification to flat sets safe, checked
# where it actually acts. Without it QuickCheck self-moves every correctly
# named file in an ordinary job, which no unit test of Identify can see.
[QuickCheck relocates every identification]
file internal/par2/quickcheck.go
--- anchor
		if !f.NeedsRename() {
			continue
		}
--- replace
		if false {
			continue
		}
--- end
