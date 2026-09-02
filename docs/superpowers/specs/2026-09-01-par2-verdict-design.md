# Identify, then verify: reordering the par2 quickcheck

**Status:** design. Nothing here is implemented.

**Supersedes:** an earlier `Par2DescribesOtherFiles` predicate, which
classified the ambiguous signature instead of removing it. Its two commits
were dropped from this branch before implementation began — see *What this
deletes* for why the distinction stops being needed. They are recoverable from
the reflog if the reasoning here is ever overturned.

**Issues:** #491 (one Assessor, one Verdict), #492 (the two call sites
diverge). Both dissolve rather than get fixed; see *What this deletes*.

**Measured at:** `18bc1ca5`, plus the live probe runs recorded below.
Reference implementation read at `../sabnzbd/sabnzbd/`.

---

## The algorithm

1. Download the main and index `.par2` files. Defer only the recovery volumes.
2. Identify every file on disk against the par2 index, by content rather than
   by name.
3. If every index entry is accounted for, rename the obfuscated files to their
   par2 names, mark the job verified, and hand it to unpack.
4. If any index entry is not accounted for, un-defer **all** recovery volumes
   and hand the job to the full par2 repair stage.

Step 1 is already what happens: `Deferred: isRecovery && opts.OnDemandPar2`
(`internal/queue/job.go:922`) is the sole writer of that field, and
`isRecovery` is true only for volumes, so the index is always fetched. Step 4
is already implemented as
`UndeferRecoveryVolumes(jobID, snap.DeferredRecoveryIndices())`
(`app.go:1484`).

The work is steps 2 and 3.

---

## Why the current code fails

### It matches on the one field the poster controls

`VerifyCRCsWithOptions` builds `par2Index` keyed on basename and runs a
name-based pass first (`verifycrc.go:129`, `:162-166`): `matchBasename`, then
`matchFlattened`. Under obfuscation neither can match, because the NZB's
subject and the par2 entry's filename are unrelated strings.

The intended fallback for exactly that case is `matchCRCSize`, and it cannot
fire. Its key is `{crc, size}` with exact size equality, but the two sides are
different quantities: par2's `FileDesc.FileSize` is the decoded length, while
both call sites supply `AssembledFile.FileSize` from `m.FileBytes(fi)`
(`app.go:1537`, `stage_quickcheck.go:137`) — the sum of yEnc-**encoded**
segment sizes.

Measured on a real release, NZB-declared against par2's recorded length:
`1.0307`, `1.0319`, `1.0320`, `1.0320`, `1.0321`, `1.0793`. Never equal, so
the fallback never matches. Its only test supplies `FileSize` from par2's own
descriptor (`verifycrc_test.go:282`), which is why nothing caught it.

Result: for an obfuscated release, identification never happens, so
verification never gets its chance — and the job reaches `Matched == 0` while
being perfectly intact.

### The content-based matcher exists and is unreachable

`par2.QuickCheckWithOptions` has four matchers, including `matchByHash16k`,
which is the right mechanism. All four sit behind a gate at
`internal/par2/quickcheck.go:67-71`:

```go
subdirEntries := filterSubdirEntries(manifest, log)
if len(subdirEntries) == 0 {
    log.Info("quickcheck: no par2 entries contain subdirectory paths — all filenames are flat")
    return nil, nil
}
```

For a par2 set whose filenames are flat — the ordinary case — none of them
run. The machinery corresponding to SABnzbd's md5of16k rename is present,
correct, and never invoked for ordinary releases.

### The ambiguous result then drives an irreversible action

`Matched == 0 && Mismatched == 0 && NoCRC == 0` (`app.go:1546`) is reached by
an obfuscated release, a Layout B par2 set, and a resumed download alike. On
that branch `DiscardDeferredPar2` (`app.go:1473`) marks every volume
`FetchNever`.

That is terminal. Post-processing cannot fetch anything: `stage_repair.go:262`
and `:275` set `job.NeedRequeue`, and `docs/post-processing-contract.md:226-230`
records in its own Open Gaps section that "nothing in `internal/app` reads it —
it is currently dead state" (confirmed: `git grep -c 'NeedRequeue' --
internal/app/` returns no files). The only two un-defer sites,
`maybeReleaseRecoveryVolumes` (`app.go:1484`) and `AckPermanentFailure`
(`workset.go:159`), both run during download.

So a healthy obfuscated download has its recovery volumes destroyed on the
strength of a matcher that could not have succeeded.

---

## Identification and verification are different operations

This is the one distinction the algorithm depends on, and the current code
conflates it.

| Question | Answered by | Source | Cost |
|---|---|---|---|
| *Which* par2 entry is this file? | `FileDesc.Hash16k` | index `.par2` | first 16 KB of the file |
| Is that file *intact*? | `FileDesc.FileCRC32` vs the assembled CRC | index `.par2` | free — computed during download |

Neither answers the other's question. `Hash16k` covers only the first 16 KB, so
it cannot verify; the whole-file CRC cannot identify, because you must already
know which entry to compare against.

They compose in one direction only: **identify by `Hash16k`, then verify by
CRC** — which now matches by name, because identification has just supplied
the name. Today we attempt them in the opposite order and the chain breaks at
the first link.

Both values come from the index `.par2`, so step 2 needs no additional
download. `FileDesc.FileCRC32` is documented as "reconstructed from IFSC
slices" (`parser.go:47`, reconstruction at `:368-398`), and IFSC packets ride
in the index rather than the recovery volumes. (The parser also has a
size-gated early exit once the FileDesc and IFSC packets are seen —
`parser.go:198-201` — which matters when parsing a volume that carries them
too, not for the index itself.)

**One caveat to encode:** `FileCRC32` is `0` when a set carries no IFSC data.
That is indistinguishable from "no CRC recorded" and must be treated as
unverifiable — identified but not verified — rather than as a match.

---

## What the four outcomes become

| Case | Identification | Verification | Action |
|---|---|---|---|
| Clean, plain names | by name | CRC matches | rename nothing, skip volumes, unpack |
| Clean, obfuscated | by `Hash16k` | CRC matches | rename, skip volumes, unpack |
| Damaged | either | CRC mismatches | fetch all volumes, full repair |
| Resumed, no assembled CRC | either | unavailable | fetch all volumes, full repair |
| Layout B (0 of N match) | nothing matches | n/a | skip volumes, fail honestly |
| Partial (some of N match) | some by name or content | n/a for the rest | fetch all volumes, full repair |

**The last two rows were one row, and splitting them is a correction this
design shipped wrong.** The original argued that a Layout B set should "fail
*after* trying, with its volumes intact" — which reads well and is false. The
volumes are never tried at all: `buildStages` registers `RepairStage` before
`UnpackStage` (`internal/app/stages.go`) and there is no second repair pass, so
the one repair this pipeline runs executes while the files those volumes
protect do not yet exist. Fetching them spends the whole recovery set to reach
the identical failure.

That was a claim about *behaviour* whose truth depended on a branch nowhere
near the sentence — the stage registration order — and naming the branch is
what settled it (`AGENTS.md` Standing Design Rule 4). **Were a post-unpack
repair pass ever added, the Layout B row becomes wrong and must fetch.**

The distinction between the two rows is `len(id.Files) == 0`, and it is safe
**only** because identification is content-based. Under the name-only matching
this design replaces, a healthy obfuscated release produced exactly the same
"nothing matched" signature, and discarding on it was the #492 defect itself.
`Hash16k` is what tells them apart, so this test may never be reintroduced
anywhere that matches on names.

---

## What this deletes

**`Par2DescribesOtherFiles` and its size tolerance.** The predicate exists to
separate Layout B from obfuscation, and that separation is only needed to
decide whether the discard is safe. Under this design no branch discards on an
*ambiguous* signature: obfuscated files are identified and pass, so the only
remaining "nothing matched" case is one where content matching has already
ruled obfuscation out. The Layout B distinction survives, but it is now drawn
by `len(id.Files) == 0` on a content-based identification rather than by a
name-and-size heuristic, which is why the predicate is still unnecessary and
both its commits stay dropped.

The size tolerance that predicate needed was correcting a comparison that
should not be load-bearing at all. Sizes are a weak proxy for identity;
`Hash16k` is a strong one, costs 16 KB, and was already implemented — the
tolerance existed only because the weak proxy had to absorb yEnc overhead the
strong one never sees.

**The `Matched == 0` guard at `app.go:1546`.** With identification working,
`Matched == 0` on a healthy job stops occurring, and where it does occur it
means "we could not account for this set", which is the fetch-and-repair
signal. The special case goes away rather than getting a better predicate.

**#492 dissolves.** The two call sites disagreed about whether `Unverified > 0`
means damage. Once identification runs, `Unverified` means what
post-processing already assumed — an entry nothing on disk accounts for — and
`par2NeedsRecovery`'s exclusion of it was a workaround for the broken matcher.

**#491 is worth re-asking, not assuming.** With the destructive branch gone,
the two call sites answer genuinely different questions — one decides whether
to fetch, the other whether to repair — and neither destroys state the other
needs. Whether they still want unifying is a smaller question than it was.

---

## What this does not delete

**`FetchNever` stays.** It remains correct for its documented meaning — "a
recovery volume the oracle **proved** unnecessary" (`progress.go:48`) — which
is now reached only from a genuine full match. The change is which branches
reach it, not the state itself.

**Leaving volumes `FetchIfNeeded` is not an option.** Given the terminal
decision point, that means never fetched *and* misleading state:
`HasDeferredPar2` stays true on a finished job, the API reports the volumes as
`"held"` rather than `"skipped"` (`internal/api/queue.go:326-330`), and a later
article failure would un-defer volumes for a job that has already finished.
Every path must end at `FetchAlways` or `FetchNever`.

---

## Work

**W1 — Split identification from relocation, then extend it to flat sets.**

Deleting the `filterSubdirEntries` early return is *not* sufficient, and would
be actively wrong. The four matchers conflate two operations: every match
immediately moves a file. `matchByBasename` (`quickcheck.go:172-198`) looks up
`flatFiles[filepath.Base(fd.FileName)]` and, on a hit, calls `relocateFile` and
records `Rename{From: basename, To: fd.FileName}`. For a **flat** entry those
two strings are equal, so un-gating naively would issue a self-move for every
correctly-named file in every ordinary job. The gate is load-bearing precisely
because the code beneath it assumes an entry whose par2 path differs from its
on-disk name.

So the shape is:

- **W1a — a pure identifier.** `Identify(dir, sets) → map[flatName]FileDesc`,
  no filesystem mutation, running name → `Hash16k` → (optionally CRC+size).
  This is the single owner of "which par2 entry is this file", which both the
  fetch decision and the rename then consume. It runs for every set, flat or
  not.
- **W1b — relocation on top.** `QuickCheck` becomes: identify, then apply a
  rename only where `From != To`. Subdirectory relocation stops being the
  precondition for identification and becomes one consequence of it.
- **W1c — the fetch decision consumes it.** `par2NeedsRecovery` calls
  `Identify` and asks whether every par2 entry was accounted for.

`shouldSkipFlatFile` (`quickcheck.go:242-248`) already skips `.par2`, `.sfv`
and `.nfo`, which is an implicit form of SABnzbd's `quick_check_ext_ignore`.
Worth making explicit when W1a is written rather than leaving it as a property
of the loop it happens to live in.

**W2 — Fix `matchCRCSize`'s size source, or retire it.** With `Hash16k`
identification in place, a CRC+size fallback earns its keep only for a set
with no IFSC data. If kept, it must compare against the real on-disk length —
SABnzbd's `is_size` is `os.path.getsize(filepath) == size`
(`filesystem.py:143`) — not the NZB's byte count. Retiring it is defensible;
leaving it as-is is not, because a dead fallback reads as a working one.

**W3 — Record the renames.** Two consumers need them and neither gets them
today. `Job.OwnedFiles` is seeded once before any stage
(`postproc.go:582-585`) and extended by four call sites —

```
git grep -n 'markRenamed(job' -- internal/postproc/ ':!*_test.go' | grep -v 'func markRenamed'
```

`stage_deobfuscate.go:48`, `stage_deobfuscate.go:58`, `stage_par2names.go:48`,
`stage_rarvolrecovery.go:114` — with quickcheck absent, while performing
exactly the operation the helper exists for. Its renames are formatted into
`job.OutputLines` (`stage_quickcheck.go:85-87`) and otherwise dropped.

The `grep -v` is load-bearing: without it the pattern also matches
`markRenamed`'s declaration at `ownedfiles.go:30`, which is how a first pass
of this enumeration reported three sites and missed `stage_rarvolrecovery.go`.

The consequence today is bounded: the ownership guard at
`extension_cleanup.go:126-130` and `sample_cleanup.go:116` returns early for an
unowned path, so an unrecorded relocation makes cleanup **skip** the file
rather than delete it. Junk left behind, never data loss. **That is inferred
from the guard's direction, not observed.**

**W4 — SETTLED: identify and rename at download-complete. The manifest does
not become mutable, because the field that must change is already mutable.**

The question was whether renaming before post-processing forces the manifest to
become writable. It does not. `FileProgress.Filename` already exists for exactly
this purpose — `progress.go:255` documents it as "the resolved on-disk filename
for file fileIdx, or empty if unresolved" — it is persisted
(`sqlite_store.go:543`, `:740`, `:1103`, `:1373`), and both par2 call sites
already prefer it over the immutable `Manifest.FileSubject`:

```go
name := m.FileSubject(fi)
if fn := p.FileFilename(fi); fn != "" { name = fn }
```

So the manifest stays immutable and keeps recording what the NZB said, while
the resolved name records what is on disk. A par2-identified rename is a change
to the second, which is what that field means.

**The owner, so this does not become a second writer.** Before this change
`Queue.SetFileFilename` had one caller, `pipeline.go:583`, which sets the name
at file registration from `filepath.Base(path)`. (That was stated here as a
live citation; it is a historical count, since this design adds the second
caller itself. There are now two call sites, `registerFile` and
`applyPar2Names` — the live enumeration, with the command that produces it,
lives at `applyPar2Names`' doc comment rather than here, where nothing runs
it.) Adding a second *independent* writer
would be the owner-model violation AGENTS.md requires escalating.

Avoid it by making the *rename operation* the owner: one function that performs
the on-disk rename and updates the resolved name together, so it is impossible
to do either alone. Per Standing Rule 2 — "when a check and an owner would both
work, take the owner" — this is preferable to writing the field at each rename
site and remembering to keep them in step.

**Two consequences worth stating, both favourable:**

- **W3 largely evaporates on this path.** `snapshotOwnedFiles` seeds
  `Job.OwnedFiles` at the start of post-processing (`postproc.go:582-585`), so a
  rename performed *before* post-processing is captured by the seed rather than
  needing `markRenamed` at all. W3 remains real for quickcheck's own relocations
  of subdirectory sets, which still happen inside post-processing.
- **~~`stage_quickcheck` starts working with no change to it.~~** *Wrong, and
  corrected during implementation.* It does read `p.FileFilename(fi)`, so it
  benefits when the resolved names are corrected upstream — but that upstream
  correction is **conditional**, which this bullet missed. On-demand par2 can
  be disabled, and a release carrying no deferred recovery volumes never
  reaches `maybeReleaseRecoveryVolumes` at all. In both cases the stage
  performs the renames *itself* and then verifies against names its own moves
  just invalidated, marking an intact job damaged.

  It cannot write the correction either: `postproc.Job` carries a
  `*queue.Job` snapshot rather than a `*queue.Queue`, so there is no writer
  behind it. The stage therefore applied the rename mapping in memory before
  verifying. This was a real change to it, not a side effect.

  **That in-memory mapping is itself gone now, and the reason is worth
  recording here rather than only in the issue it produced.** It was correct,
  but it was a *second* enforcement point for an ordering `internal/app`
  enforced separately — and the two together were four different patches for
  one root cause. #494 replaced them with `par2.Assess`, which reports renames
  without applying them and hands them out only alongside a verdict computed
  from pre-rename state. Both callers now assess first and relocate second, so
  there is no window in which the names are wrong and nothing to remap.

**One ordering fact this depends on**, checked rather than assumed:
`quickcheck` is the first stage, ahead of every renaming stage
(`rar_volume_recovery`, `recover_par2_names`, `deobfuscate` — see the stage
table in `docs/post-processing-contract.md:74-87`). So nothing reads
`FileProgress.Filename` after those stages rename files, which is why their
not updating it is tolerable today and stays out of scope here.

**W5 — Give the user an accurate failure message.** A job that fails because
its par2 protects files the NZB never delivered should say so. This is the
"flag what happened so they can fix it manually" requirement, and it is the
one place the Layout B distinction still has value — as a message, where a
wrong answer costs a confusing sentence rather than a job.

### Sequencing

W4 is settled, so the order is:

1. **W1a** — the pure identifier. Independently testable against synthetic sets
   and the only piece the rest depends on.
2. **W1b** — relocation on top, preserving today's subdirectory behaviour. The
   existing quickcheck tests are the regression net; a self-move for a
   correctly-named flat file is the specific failure to pin.
3. **W1c + W4's rename owner** — wire the fetch decision to `Identify`, and add
   the rename-plus-resolved-name owner. These land together because the fetch
   decision and the rename are the two consumers of the same result.
4. **W2** — fix or retire `matchCRCSize`. Independent of the above; it only
   matters for a set with no IFSC data once `Hash16k` identification works.
5. **W3** — the residual `markRenamed` omission for subdirectory relocations.
6. **W5** — the failure message. Independent throughout.

**Empirical grounding for steps 1–3.** `scripts/nzbprobe -full` runs the whole
proposed algorithm against a real obfuscated release and real servers. On
`Kamelot-Dark.Asylum-WEB-2026-ENTiTLED`:

```
=== PROPOSED: phase 1 — identify by Hash16k (16 KB per file)
7xq6N6P340dCh9Lnih5hY3jsArfSN1     a0576589b6c623ac9bc777a81c7b9515.… MATCH
  identified 1 of 1 delivered content file(s)
  every par2 entry accounted for: true

=== PROPOSED: phase 2 — verify by CRC32 (downloading full payload)
a0576589b6c623ac9bc777a81c7b9515.… ed5d57f7   ed5d57f7  VERIFIED

  -> quickcheck PASSES: rename the identified files, skip recovery volumes, unpack
```

A 125 MB payload identified from 16 KB and verified against a 3,696-byte index,
where the current code reaches `Matched=0 Unverified=1 NotInPar2=6` and
discards the recovery volumes. `noPar2CRC=0` confirms this set carries IFSC
data, so the free-verification half is real and not merely assumed.

**Two limits of that run**, so it is not read as more than it is: the probe
computes the CRC by assembling the payload and calling
`crc32.ChecksumIEEE`, where production combines per-article yEnc CRCs — so it
proves par2's recorded CRC matches the file's true CRC, not that our assembled-CRC
pipeline reproduces it. And its section on the shipped verifier passed
`CRC32: 0` throughout, modelling a resumed download; the claim that real CRCs
would not change those counters was derived from source, not observed. (That
section has since been deleted along with the name-matching verifier it
measured — see the note in its place in `scripts/nzbprobe`.)

---

## Costs accepted

**Fetching every recovery volume on any mismatch** is more bandwidth than
computing the exact block deficit. SABnzbd takes the deficit route by default
(`enable_all_par = False`, `cfg.py:458`), falling through to
`par2cmdline_verify` to learn the shortfall — but that depends on
post-processing handing work back to the downloader, which is `NeedRequeue`,
which has no consumer here. Fetch-all is the only single-pass option, and
on-demand par2 keeps its real win: a clean download still fetches nothing.

Implementing `NeedRequeue` promotion would retire this cost and is already a
documented Open Gap. It is larger than W1–W5 and is not a prerequisite.

**16 KB read per unidentified file.** Negligible against the download, and
incurred only where a name did not already match.

---

## Verification status

| Claim | Basis |
|---|---|
| Size ratios; the observed divergence on a real release | Live probe run against `Kamelot-Dark.Asylum-WEB-2026-ENTiTLED.nzb.gz` |
| Only recovery volumes are deferred | `job.go:922`, sole writer of `Deferred` |
| Whole-file CRC available from the index alone | `parser.go:47`, `:368-398`, `:198-201` |
| Matchers gated behind subdirectory entries | `quickcheck.go:67-71` |
| `matchCRCSize` cannot fire | Source, plus both figures measured |
| `NeedRequeue` has no consumer | `git grep` returning zero files in `internal/app/`, plus the contract's Open Gaps entry |
| Two un-defer sites, both during download | `git grep -n 'UndeferRecoveryVolumes\|undeferRecovery'` |
| Quickcheck omits `markRenamed` | The enumeration under W3 |
| W3's cleanup consequence | **Inferred** from the guard's direction, not observed |
| SABnzbd's mechanism and defaults | Read from source, cited by line |
