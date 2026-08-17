# Article Validation Contract

> **Status: proposal; dispositions settled.** None of the assertions below are
> implemented as stated yet, but §8 is no longer open — what each class of
> violation produces has been decided and is binding on the work that follows.
> This
> document defines what GoNZBD asserts about a Usenet article, where each
> assertion belongs, and what it deliberately does not assert. It exists to
> replace case-by-case defensive coding with a stated invariant set.

Read this before touching `internal/nzb`, `internal/nntp`, `internal/decoder`,
the decode/reconcile path in `internal/downloader`, or the accept path in
`internal/assembler`.

## Why this document exists

A cluster of defects — #379, #384, #386, #387, #392 — are the same defect
wearing different hats: **a real invariant that was never stated, so every
layer wrote speculative tolerance code against a state that could not occur.**

#386 is the worked example. The NZB parser has always dropped segments with an
empty Message-ID, so an article with no Message-ID cannot reach the assembler.
The assembler nonetheless carries empty-ID handling across six functions. That
machinery is untestable through any real input path, so it was never exercised,
so it was wrong — and a fix to it shipped a regression that converted a stuck
job into a silently truncated file.

The lesson generalises:

> **An invariant that is true but unenforced is worse than one that is false.**
> A false invariant fails loudly the first time it is violated. A true but
> unstated one grows defensive code for a state that never arrives, and that
> code is never executed, never tested, and eventually wrong.

The remedy is not more validation. It is validation placed **once, as far out
as it can be decided**, and stated loudly enough that inner layers may assume
it.

## 1. Threat model — state it before choosing controls

Several existing comments describe article headers as "attacker-controlled" and
justify checks on that basis. The framing is correct but compressed, and stating
it fully changes which *further* controls are worth writing.

**A CRC32 is not an adversarial control.** It detects accidental corruption. A
hostile server can forge a CRC32 collision trivially — it simply computes the
checksum over whatever bytes it wants to send. Any check whose stated purpose is
"defend against a malicious server tampering with content" and whose mechanism
is CRC32 does not do that job.

The honest model, and the one this contract adopts:

| Aspect | Server / poster is | Therefore |
|---|---|---|
| **Content correctness** | **Trusted** | We cannot detect a poster who encoded the wrong bytes. par2 is the only oracle. Do not write controls that pretend otherwise. |
| **Resource consumption** | **Untrusted** | Bound every allocation, offset, file size, and loop derived from server input. A malformed header must not cost unbounded memory, disk, or time. |
| **Identity** | **Verifiable** | We asked for a specific Message-ID. Whether the response corresponds to it is decidable and must be checked. |

`maxPartOffset` and `offsetOutOfRange` already sit correctly in row 2, and their
comments already say so — `internal/decoder/decoder.go` names the assembler's
`ExpectedSize` check as "the authoritative bound", and
`internal/assembler/assembler.go` scopes itself to "the sparse file itself is
still the attack". Neither claims to bound content correctness. The value of
writing the table down is not to correct them but to stop the *next* control
from being written in row 1, where nothing can succeed. And it is why the
missing Message-ID
response check matters more than either: identity is the one adversarial
property we can actually settle.

## 2. Classify every claim before deciding where to check it

Every field in an NZB or an article is a **claim** by some party. Classify it,
and the correct handling follows mechanically.

### Class 1 — Self-verifying

The article carries its own proof. Decidable with no external reference.

- yEnc `=yend size=` versus the decoded byte count
- yEnc `pcrc32` / `crc32` versus CRC32 of the decoded bytes
- NNTP dot-stuffing and terminator well-formedness
- NZB XML well-formedness

**Rule: verify always; a failure is terminal for that copy and retryable on
another server.** Already implemented correctly in `internal/decoder`. This tier
is in good shape and needs no work.

### Class 2 — Cross-checkable

Two independent sources assert the same fact. Disagreement is *detectable*;
which side is wrong may or may not be decidable.

| Claim | Sources | Which side is authoritative |
|---|---|---|
| Message-ID of the served article | our request, the `222` response line | **Ours.** Fully decidable. |
| Part ordinal | NZB `<segment number=>`, yEnc `part=` | Neither. Count it. |
| Total file size | NZB sum of `bytes`, yEnc `=ybegin size=` | Neither; NZB is encoded bytes, yEnc is decoded. |
| `=ybegin size=` across parts of one file | each part of the same file | **Must be identical.** Disagreement is proof of malformation. |
| `=ybegin name=` across parts of one file | each part of the same file | Same. |
| Part byte ranges | all parts of one file | **Must tile `[0, size)` with no gap or overlap.** |
| `=ypart begin=`/`end=` vs decoded length | **the same article**, internally | **Decidable within one article.** `end - begin + 1` must equal the decoded length, and the 0-based `offset + len` must not exceed `=ybegin size=`. |

**Rule: where one side is authoritative, enforce. Where neither is, the
disagreement is itself the finding — raise a job-level warning naming both
claims and continue** (§8, decision 2). Never reject on a Class 2 disagreement
alone: neither side is provably wrong, and refusing on a coin flip destroys data
par2 could have used.

Rows 4–6 are the strongest untapped signal in the system: they need no external
oracle, only the file's own parts checking each other. **Row 7 is stronger
still** — it needs only the article checking *itself*, and is therefore decidable
at L2 rather than L4. See C5.

### Class 3 — Unfalsifiable

One source, no oracle, not covered by any checksum.

- yEnc `=ybegin name=` — the filename. Note it is also *unconsumed*: nothing
  outside `internal/decoder` reads `Article.Filename`, and the displayed name
  comes from the NZB subject. Do not build controls on it (§5.D).
- yEnc `=ybegin size=` — total size, considered alone
- NZB `<file subject=>` and everything derived from it — but see
  `fsutil.SanitizeFilename`, which already strips control characters, path
  separators, shell metacharacters and Windows device names before it reaches
  any filesystem call. This surface is covered; do not add a second control.
- posting date

**`=ypart begin=` does not belong on this list.** It is cross-checkable against
two values the same article already carries (§2, Class 2, row 7). Treating it as
unfalsifiable pushes its only file-relative bound out to L4, three layers further
in than necessary.

**Rule: these cannot be verified, only bounded.** Guard the *consequences*
(unbounded allocation, arbitrary apparent file size, writes outside the file),
never pretend to guard the value. Then hand off to par2.

## 3. The load-bearing fact about yEnc

> **yEnc's checksum covers the decoded payload and never the header.**

A CRC-valid article proves its **bytes** are intact. It proves nothing about
*where they go*, *which file they belong to*, or *how large that file is*.
Every header field is Class 2 or Class 3. **None is Class 1.**

That single sentence explains #379, #384, #386 and #387. Each is "we trusted a
header field the checksum does not cover."

**Corollary: a passing CRC is not a licence to trust the geometry.** Geometry
needs its own consistency argument. Two are available, and neither comes from
the checksum: the article's own `begin`/`end`/`size` triple (decidable at L2,
C5) and cross-part tiling (decidable at L4, E3/E4). Naming only the second is
what places every geometry bound at L4.

### What to assume about yEnc specifically

yEnc is **not an IETF standard.** It is a de-facto format (yenc.org draft v1.3,
2002) with no conformance suite and known-divergent implementations in the
wild. "The spec says" is a far weaker argument for yEnc than for NNTP or
Netnews. This produces a deliberate asymmetry:

- **Be permissive about yEnc syntax.** Unknown keys, odd spacing, missing
  optional fields, a `part=` that is non-numeric, `begin=0` where the format is
  nominally 1-based — tolerate all of it. Real posters emit all of it, and
  articles that download correctly today must keep doing so. The existing
  decoder already gets this right and its tolerances should not be tightened.
- **Be strict about yEnc-derived geometry.** Offsets, lengths, and sizes decide
  where bytes land on disk. Permissiveness there is not compatibility, it is
  data loss. This is the half that is currently too loose.

Do not conflate the two. Rejecting an article because its header had an
unrecognised key is a compatibility regression; accepting an offset that
overlaps a durable range is a correctness one.

## 4. The placement rule — and the tier above it

Before applying the ladder, ask whether the ladder is needed at all.

> **Prefer making a violation unrepresentable over checking for it.**
> An enforced invariant costs a check, a test, and an error path — three things
> that can be wrong. A structural one costs nothing and cannot be bypassed.

The empty-Message-ID saga is the worked example, and the obvious remedy is the
wrong one. Enforcing "every article has a Message-ID" at L0 so the assembler can
assume it does work — but the assembler does not need Message-IDs at all:
it keys `seenDone`/`seenFailed` on `msgID` when every `WriteRequest` already
carries `ArtIdx`, a globally unique `int32` that cannot be empty. **Re-keying
those maps deletes the same six functions' worth of machinery without enforcing
anything, without an L0 change, and without a new error path.** §5.F carries
this class.

The test: *if the invariant can be violated, is that because the data model
permits it, or because the world does?* Only the second kind belongs on the
ladder.

**The tier has a precondition, and F1 sits right on its edge.** It works only
when the replacement identifier has **no representable-but-invalid value**.
`""` is not a valid Message-ID, so an empty-string key is a loud, checkable
error. `0` *is* a valid `ArtIdx`. Re-keying therefore trades a noisy guard for a
silent alias: a struct-zero `WriteRequest` reaching the accept path would
address article 0 rather than trip anything. Today that is excluded by the
`FileIdx` sentinel check, **not by the type** — which is an argument for doing
F4 (splitting control messages out of `WriteRequest`) alongside F1 rather than
after it. Where the substitute type has a valid zero, the tier converts a
detectable failure into an undetectable one unless something else makes the
zero unreachable.

A claim is **decidable at layer L** if L holds all the information needed to
prove it false.

> **Enforce each claim at the outermost layer where it is decidable, exactly
> once, and let every inner layer assume it.**

Every layer that could enforce and does not must instead **tolerate**, and
tolerance code is where these bugs live.

| Layer | Component | Sees | Can decide |
|---|---|---|---|
| **L0** | `internal/nzb` | the whole document, offline, pre-network | everything cross-segment and cross-file; all Message-ID syntax |
| **L1** | `internal/nntp` | one request/response pair | response identity, wire framing, size caps |
| **L2** | `internal/decoder` | one article body | all Class 1 payload checks, **plus any Class 2 claim whose two sources are both inside one article** (C5) |
| **L3** | reconcile (`internal/downloader`) | article **and** its manifest entry | every Class 2 NZB-vs-article disagreement |
| **L4** | `internal/assembler` | all parts of one file | tiling: overlap, gaps, bounds |
| **L5** | `internal/par2` | cryptographic hashes | the only real content oracle |

L0 is the highest-leverage layer and the most under-used: it is offline, it has
the entire document in hand, and it is the **only** layer that can see
cross-segment structure before a single byte is fetched. Everything L0 rejects
is a class of downstream tolerance code that can be deleted outright.

## 5. Base assumptions — the enforceable set

**Entry point.** Every assertion in this section, its layer, what a violation
produces, and what it waits on. Read this table first; the subsections argue for
the rows.

| # | Assertion | Layer | Violation produces | Blocked on |
|---|---|---|---|---|
| A1 | Message-ID non-empty | L0 | reject, counted **in place** (before the digest) | — |
| A2–A5 | Message-ID RFC syntax: ≤250 octets, no WSP, no `<`, printable ASCII | L0 | reject, counted after the digest write | — |
| A6 | Message-ID contains `@` | L0 | **count only** | evidence |
| A7 | Message-ID unique job-wide | L0 | reject the later part, counted | — |
| B1 | `222` Message-ID equals the requested one | L1 | fail article **and drop the connection** | — (see F5) |
| C1–C3 | payload self-verification | L2 | ✅ already enforced | — |
| C4 | checksum presence | L2 | count only | — |
| C5 | `offset + len ≤ size`; `end − begin + 1 == len` | L2 | fail article (`end=` half counts first) | — |
| D1–D3 | NZB ↔ article disagreements | L3 | job-level warning | — |
| E1–E2 | bounds, exact-offset collision | L4 | ✅ already enforced | — |
| E3 | range overlap | L4 | **telemetry only** | A7 **and** E5 |
| E4 | part tiling / gaps | L0 + L4 | warn at ingestion | — |
| E5 | UU body only satisfies a single-segment file | L3 | reject | — |
| F1–F5 | structural (§5.F) | — | nothing — the state stops existing | — |

**Build order.** The F-items land first (§5.F), then the assertions:

> F3 → F2 → F1a (fixture `ArtIdx`) → F1b (flip the key) → F5/B1 → C5 → E5 → A1–A7 → E3/E4

F3 leads because it is the smallest instance of the pattern and the cheapest
place to learn its cost; doing it first is what established that the two-commit
split in §5.F applies to the whole class rather than to F1 alone.

F4 is deliberately unscheduled: it shares one channel with article requests and
the cancel path depends on that channel's FIFO ordering, so it is a larger change
than its row suggests and is not a prerequisite for anything.

These are the assertions this contract proposes GoNZBD make. Each names the
layer that owns it and its current status. Verified against the tree at
`b2793dc1`; the ⚠ rows were confirmed by probe, not by reading.

### A. Message-ID (L0 — NZB parse)

RFC 5536 §3.1.3 requires exactly one Message-ID per article. RFC 3977 §3.6
requires it to be 3–250 octets, printable US-ASCII only, begin `<`, end `>`,
and contain `>` nowhere else. RFC 5536 additionally requires `id-left@id-right`
and forbids whitespace inside.

| # | Assertion | Status |
|---|---|---|
| A1 | Message-ID is non-empty | ✅ enforced — but **silently dropped**, not counted or reported |
| A2 | ≤ 250 octets | ⚠ **absent** — a 402-octet ID parses and is dispatched |
| A3 | no whitespace (SP, HT), no CR/LF/NUL | ⚠ **partial** — L1 rejects CR/LF/NUL as an *injection* guard; SP and HT pass and are emitted to the wire |
| A4 | no `<` or `>` inside | ⚠ **half-enforced** — `validateMessageID` rejects `>` anywhere after trimming the wrapper; only an embedded `<` leaks |
| A5 | printable US-ASCII only | ⚠ **absent** |
| A6 | contains `@` | ⚠ **absent** — `no-at-sign` parses and is dispatched |
| A7 | unique across **every segment of the whole NZB**, not merely within one file | ⚠ **absent** — the same ID on two part numbers yields two articles fetching identical bytes to two different offsets, a **guaranteed** overlap |

**A1–A5 and A7 are rejected at L0** with a counter, exactly as `BadArticles`
already works — the segment does not become an `Article` and is never
dispatched. A1–A5 are pure syntax violations that could not produce a successful
fetch anyway (the wire request itself would be malformed). A7 is not a syntax
violation but is rejected on a stronger ground: two segments sharing one
Message-ID fetch identical bytes to two different offsets, so accepting both
guarantees an overlap. Reject the **later** part number and count it, keeping
the first occurrence.

**A1 stays, but it is no longer load-bearing.** It is still worth converting the
silent drop into a counted rejection, because a silent drop is how #392's
machinery came to exist (§7). But F1 deletes that machinery structurally, so A1
is now hygiene rather than a prerequisite — do not sequence F1 behind it.

**A7 is job-wide, not per-file**, because two structures downstream key on
Message-ID with no file scoping and one with no job scoping at all:

- `Manifest.messageIDIndex` (`internal/queue/manifest.go`) is a **job-wide**
  `map[string]int` built last-writer-wins. One Message-ID appearing in two
  *files* of one NZB silently makes `MarkArticleEmitted` / `ClearArticleEmitted`
  act on the wrong article. Per-file uniqueness does not close this.
- `dispatchTracker.tryList` and `.inFlight` (`internal/downloader/tracker.go`)
  were keyed on Message-ID **alone**, with no job scoping. Two resident jobs
  sharing a Message-ID shared one try-list, so one job's article could exhaust
  its servers without ever having been fetched for the other.

The second is out of A7's reach entirely — two jobs, not one NZB — so ingestion
could never have fixed it. It is closed structurally instead, by F3, which keys
the tracker on `(jobID, artIdx)`. The first is F2's to remove on the same terms.

Both are worth stating as the pattern rather than the incidents: **a
Message-ID-keyed structure whose scope is wider than one job is a bug waiting for
a duplicate**, and A7 tightening ingestion is a narrower remedy than changing the
key. Where both are available, prefer the key (§4).

**A6 is counted, not rejected** — the one deliberate exception to decision 1,
and the reason is #379's. An ID lacking `@` violates RFC 5536 but remains
*fetchable*: unlike A1–A5 it produces a well-formed wire request that a server
may well answer. Rejecting it would fail articles that download correctly today,
across every server, on zero evidence about how often real posters emit it.
Count first; promote to rejection once the counter says the case is real and
rare.

### B. Response identity (L1 — NNTP)

| # | Assertion | Status |
|---|---|---|
| B1 | the `222` response's Message-ID equals the one requested | ⚠ **absent — highest-value single change** |
| B2 | body size ≤ 10 MB | ✅ enforced |

**B1 is the most important gap in the system.** `internal/nntp/pipeline.go`
matches responses to requests by **FIFO order only**. The `222 <n> <message-id>`
line is parsed into `cmdResult.line` and never compared. A single desync — one
unsolicited response, one server that answers out of order — silently
mis-attributes *every subsequent article on that connection*, writing correct,
CRC-valid bytes into the wrong file at the offset their own header declares.
Nothing downstream can detect this, because each article is internally
consistent.

It is one string comparison on data already parsed, and it is the assumption
every layer from L2 outward silently depends on.

**B1 is better built as F5 (§5.F) than as an assertion.** Matching is only
needed because `runReader` pops the FIFO blind; carrying the requested
Message-ID on `pendingCmd` and matching there makes a desync *unrepresentable*
rather than detected. Note also that `popPending() == nil` already catches an
unsolicited response arriving on an **empty** FIFO — the live gap is specifically
pipelining depth > 1, which is the case worth stating in the change. And the
`222` line arrives as one undivided remainder string, so this is field-splitting
plus angle-bracket normalisation, not a bare string compare.

**A mismatch must drop the connection, not merely fail the article.** A
disagreement is proof the FIFO is desynced, which means the *next* response is
mis-paired too. Failing one article and continuing would catch the first
casualty and silently produce all the rest. `finishReader` already exists for
exactly this and is the correct exit.

### C. Payload self-verification (L2 — decode)

| # | Assertion | Status |
|---|---|---|
| C1 | `=yend size=` equals decoded length | ✅ enforced |
| C2 | `pcrc32`/`crc32` matches, **when present** | ✅ enforced |
| C3 | offset and allocation are bounded | ✅ enforced |
| C4 | whether a checksum was present at all is **counted** | ⚠ **absent** |
| C5 | the article's own `begin`/`end`/`size` triple is self-consistent | ⚠ **absent** |

C4 is the "mark it" principle (§7) applied to the most common real gap: a
poster who omits `pcrc32`. The decoder correctly notes that reaching the
assembler does not imply a checksum was verified, only that none disagreed —
but that distinction is discarded, so nothing downstream can tell a verified
article from an unverified one. **A counter, not a persisted field**: nothing
downstream branches on it today, and §7's evidence ordering never consults it.
Persist it only when a consumer exists.

**C5 is the cheapest unclaimed check in the document.** `parseHeader` parses
`begin` and ignores `end` entirely — the `=ypart` key-value loop handles only
`begin`. Two identities follow from fields the decoder already has in hand:

- `hdr.offset + len(decoded) <= =ybegin size=` — always checkable. State it on
  the **0-based `hdr.offset`**, not on `begin` directly: `parseHeader`
  deliberately tolerates `begin=0` and leaves `offset` at 0, so a `begin-1`
  formulation is off by one for exactly the input the tolerance exists to
  accept.
- `end - begin + 1 == len(decoded)` — checkable whenever `end=` is present.

Both are Class 2 *within a single article*, so they need no manifest, no sibling
part, and no external reference. Enforcing them at L2 gives every article a
tight, self-derived geometry bound. Today the only **file-relative** bound is
`offsetOutOfRange`'s `ExpectedSize + 12.5%` at L4, which is blind for any NZB
that declares no size (`ExpectedSize <= 0` returns early and checks nothing
beyond sign and overflow). C3's `maxPartOffset` is a real L2 bound but an
absolute one — it stops `MaxInt64`-shaped garbage, not an offset that is merely
wrong for this file.

Per §3, the `end=` half is **counted before it is enforced**: `end=` is optional
in a format with no conformance suite, and divergent emission is likely.

### D. NZB ↔ article reconciliation (L3 — mostly missing)

| # | Assertion | Status |
|---|---|---|
| D1 | served `part=` equals the requested segment number | ⚠ counted only (#379) |
| D2 | `=ybegin size=` identical across all parts of a file | ⚠ **absent** |
| D3 | decoded length is plausible against the NZB's `bytes` | ⚠ **absent** |
| ~~D4~~ | ~~`=ybegin name=` identical across all parts of a file~~ | **dropped as ceremony — see below** |

D2 is Class 2 with **no authoritative side needed**: the parts of one file
disagreeing with each other is proof of malformation on its own terms,
detectable with one stored value per file and no external reference.

**A `name=` cross-part consistency check is deliberately NOT proposed.**
`decoder.Article.Filename`
has **zero consumers outside `internal/decoder`** — the displayed name is derived
from the NZB subject in `convertFile`, and the decoded `name=` is parsed and then
discarded. The check would have plumbed a field through three packages in order
to warn about a value the system never uses. `Article.TotalSize` is likewise
unconsumed today, but C5 gives it a real job, so it stays.

The general rule this yields, worth applying before any future check is added:
**a consistency check on a field with no consumer is ceremony.** Either the field
earns a consumer or it should be deleted, and a warning is not a consumer.

**Both surviving assertions produce a job-level warning and nothing else**
(§8, decision 2). Neither rejects an article, fails a file, or feeds
`isRetryableDownloaderError`. The warning names both claims and the file they
disagree about, so a user reading the job can see that the posting is malformed
before par2 tells them.

D3 additionally **gains no authority over `ExpectedSize`** (§8, decision 4). It
observes that the decoded length disagrees with the NZB's `bytes` and says so;
it does not tighten the 12.5% slack in `offsetOutOfRange`, and the NZB's `bytes`
remains advisory everywhere it is already advisory.

### E. Geometry (L4 — assembler)

| # | Assertion | Status |
|---|---|---|
| E1 | offset ≥ 0, no overflow, within `ExpectedSize` + 12.5% | ✅ enforced |
| E2 | no two articles share an exact start offset | ✅ enforced (#385) |
| E3 | no two articles' ranges **overlap** | ⚠ **absent** (#387) |
| E4 | the parts tile `[0, size)` with no gap | ⚠ **absent** at L4; also undetected at L0 |

| E5 | a UU-decoded body only satisfies a single-segment file | ⚠ **absent** (#346) |

**E3 lands as telemetry, not a hot-path guard** (§8, decision 3), but that
decision rests on A7 *plus* E5, not on A7 alone. A7 does not close the UU route,
and that route is live today:

`decodePayload`'s yEnc-failure fallback returns `offset: 0` unconditionally,
with a comment reasoning that UU is "single-part by construction". That is a
true statement about the *format* standing in for a check on *this request*. If
a server answers segment 5 of a multi-part file with a body that fails yEnc
parsing but succeeds at UU, those bytes are written at offset 0 — over segment 1,
a **different** article, which A7 cannot touch because no Message-ID is repeated.
This is #346, already filed; what is new here is that it is load-bearing for
decision 3.

E5 is decidable at L3 with no new data: the requested segment number is already
on `articleRequest`, and a UU decode that satisfies a request for part > 1 is
refusable on the spot. **Take E5 before relying on decision 3.**

With A7 and E5 both in place, what remains does not justify a range query on
every accept. Count overlaps, warn on them, and revisit only if the counter
shows a residual population neither explains. This also defers #387's design
choice — the interval structure it debates is only worth building if that
counter says so.

E4 has an L0 half worth taking first: **a gap in NZB part numbers is decidable
offline.** A file whose segments are numbered 1, 2, 4 parses today with no
error, no counter, and no warning, and assembles into a file with a hole. We
know at parse time that this posting is incomplete and will require repair.
Saying so at ingestion is free.

### F. Structural invariants — violations made unrepresentable (§4)

These are not assertions. Nothing checks them, nothing can fail them, and they
need no error path. They are changes of representation that delete the states
the rest of this document would otherwise have to guard.

| # | Change | Deletes |
|---|---|---|
| F1 | key `FileWriter.seenDone`/`seenFailed` on `ArtIdx int32`, not `msgID string` | `resolvedUntracked`, `giveBackUntrackedPart`, the `resolvedUntracked` consult in `handleSuccessArticle`, and every `msgID == ""` early return in `fail` / `failPermanent` / `failDisplaced` — i.e. #392 |
| F2 | call the by-index `MarkArticleEmittedByIdx` / `ClearArticleEmittedByIdx` everywhere | `Queue.MarkArticleEmitted`, `Queue.ClearArticleEmitted`, `Manifest.articleIndexByID`, `buildMessageIDIndex`, `dropMessageIDIndex`, the `messageIDIndex` field, three eager build sites |
| F3 | key `dispatchTracker.tryList`/`.inFlight` on `(jobID, artIdx)`, not bare `messageID` | three `//nolint:unparam` directives, and the cross-job try-list aliasing bug |
| F4 | split control messages out of `WriteRequest` | the `FileIdx` sentinels, the `JobID == ""` discrimination, the "control message convention" comment class |
| F5 | carry the requested Message-ID on `pendingCmd` and match it in `runReader` | makes B1 structural rather than an assertion — see §5.B |

**Implementation planning for these lives in their issues, not here.** Each
carries a blast radius, an ordering constraint and a red-green argument that are
commit-body material; three review rounds established that predicting them in a
contract document produces confident, wrong text. Two constraints are contract-
level and stated here because getting them wrong is silent:

- **Every F-item that tightens a fixture-visible key is two commits, not one.**
  First give the affected test literals a distinct, explicit index, landing green
  under the *unchanged* key. Only then flip it. The first commit is provably
  inert, which is what makes a red check possible at all — fused into one commit,
  the fixture rewrite and the behaviour change are indistinguishable.

  For F1 that is roughly 80 `WriteRequest` literals across `internal/assembler`
  and `internal/app` which set no `ArtIdx` today; under an `int32` key they all
  alias article 0, and two such articles in one file silently stop the file
  reaching `TotalParts`.

  **This is a property of the class, not a quirk of F1.** F3 was described here
  as contained and needed the same split: `fakeArticle` left `ArtIdx` at the
  struct zero, and two tests drove `fetchArticle` concurrently with two requests
  under one job and no index, which the new key collapsed into a single tracker
  entry. Both tests would have kept passing while no longer exercising what they
  name. Assume the split is required until a fixture audit says otherwise.

  A second effect to expect, also seen in F3: tightening a key retroactively
  exposes fixtures that were passing for the wrong reason. Two tests seeded the
  try-list under one job while exercising a request carrying another; the looser
  key matched regardless, so the mismatch was invisible. Budget for fixing tests
  that were never testing what they claimed.
- **F1's empty-ID deletions are not independently landable.** `fail`'s early
  return on an empty Message-ID exists so `giveBackUntrackedPart` is not charged
  twice; deleting one without the other double-counts or under-counts
  `partsWritten`. They go in the same commit.

`ArtIdx == 0` is a valid index where `""` was not a valid Message-ID (§4), so F1
trades a loud key error for a silent alias. F4 does **not** mitigate that — it
removes control messages, not unset article ones. The mitigation is the
two-commit split above, and it is the reason that split is mandatory rather than
stylistic.

None of the five alters behaviour for well-formed input, none depends on any §8
decision, and none needs an L0 change first. **They land before the assertions**,
because every assertion written first would guard states these make unreachable.

## 6. What we cannot guard against

Being explicit here is what keeps the rest of the contract honest, and stops
future work from adding controls that cannot succeed.

1. **A poster who encoded the wrong bytes correctly.** The CRC is computed over
   their wrong bytes and passes. → par2 only.
2. **A wholly wrong but internally consistent geometry.** If every `begin=` in a
   posting is shifted by the same amount, tiling checks pass and the file is
   garbage. → par2 only.
3. **An honestly-described truncated posting.** The NZB accurately describes
   what was posted; what was posted is incomplete. → par2 only — **unless the
   NZB's own part numbering declares the gap**, which E4's L0 half detects at
   ingestion. The unguardable case is a posting that is short without saying so.
4. **Wrong or obfuscated filenames.** Not an integrity problem. → unrar/par2
   rename.
5. **Content forged by a hostile server.** CRC32 is not a MAC (§1). Only par2's
   hashes have any adversarial standing, and only for files that ship par2.

Anything on this list must **not** acquire a control that claims to address it.
The correct response to all five is the same: assemble, mark, and let post-
processing adjudicate.

## 7. "Corrupt on disk is fine, as long as we mark it"

Assembly is **placement and bookkeeping**, not adjudication. The assembler's job
is to put bytes where the article says they go and to record accurately what it
placed and how well-evidenced it was. par2 decides whether the result is right.

This argues *against* reflexive rejection. Refusing an article we cannot verify
discards data that par2 might well have been able to use, and converts a
repairable file into an unrepairable one. But it does not argue for accepting
everything, because writing an unverifiable article can destroy a better-
evidenced one (#387: an overlapping write silently falsifies a Class A fact that
has already been acked durable).

The rule that resolves the family:

> **Write it and mark it — unless writing would destroy something
> better-evidenced. Then refuse it and mark that.**

"Better-evidenced" needs a concrete ordering, and **the one available in-band
has two levels, not four**:

> **written-or-reported beats accepted.**

That is exactly what `offsetOwner{id, written}` records and what
`offsetSettledBy` already consults. A collision between two merely-accepted
articles is a coin flip and either may be written; a collision with a written
range is not, and must be refused.

**The durable tier is deliberately collapsed into "written", because it is not
retrievable where the decision is made.** `ArticleFact` carries only
`FileIdx`, `ArtIdx`, `Offset`, `Length` and `CRC32` — it has no evidence tier,
and its own package doc says Class A asserts nothing about presence on disk. The
durable bit lives in `FileExtent.Durable` in the ExtentStore, which
`internal/assembler` does not import at all: reaching it from `FileWriter.Accept`
would mean a SQLite read on the single goroutine that owns every open file
handle, on the hot accept path.

So the four-level ordering that reads naturally here — durable-and-acked beats
written beats accepted beats claimed — **does not exist and must not be cited as
though it does.** Anything wanting it must first push an in-memory acked set down
from the barrier, which is a design change with a cost, not a lookup. #387's
overwrite of an acked-durable range is the case this concession leaves open, and
that is the honest statement of the limit.

Refusal is never silent. Every refusal produces a recorded, user-visible
disposition — which is exactly what #382's `resolve(article, disposition)`
enumeration is for.

## 8. Settled decisions

Decided; binding on the work that follows. Recorded with their reasoning so a
later reader can tell a decision from an oversight.

1. **Reject, don't report.** A1–A5 and A7 are refused at L0: the segment never
   becomes an `Article` and is never dispatched, with a counter on the parse
   result alongside `BadArticles`. Rejection is cheap here because these
   segments could not have produced a usable article anyway. **A6 is the single
   exception** — it is counted, not rejected, because unlike the others it is
   still fetchable and no evidence exists about its frequency in the wild
   (see §5.A). **A7 is scoped job-wide, not per-file**: the downstream
   structures that a duplicate Message-ID corrupts are job-wide.

   **Where in the loop matters, and getting it wrong is silent.**
   `partitionSegments` folds every structurally-valid article ID into the
   SABnzbd-compatible MD5 digest *before* it applies the dedup and size
   rejections — deliberately, and the comment there says so. Any new rejection
   that skips the `digest.Write` changes `NZB.MD5` for every affected document,
   breaking duplicate-job detection (`ExistsByMD5`), history MD5 search, and
   cross-implementation parity with Python SABnzbd. **New rejections must sit
   after the digest write**, exactly where the `bytes` rejection already sits.
   Nothing fails loudly if this is wrong.

   **A1 is the exception and must stay *before* the digest**, where it already
   is. Empty IDs are dropped ahead of the digest write deliberately, mirroring
   Python. Converting A1's silent drop into a counted rejection must not move
   it — count in place. Applying the rule above to A1 would change `NZB.MD5` for
   every affected document, which is the breakage the rule exists to prevent.

2. **A Class 2 disagreement is a job-level warning.** Not telemetry alone —
   the user is the party who can act on "this posting is malformed" by finding
   another one. Not a rejection either: no side is provably wrong. The warning
   names both claims and the file. This covers D1–D3 and the E3 counter.

3. **Overlap detection is telemetry.** E3 counts and warns; it does not guard
   the accept path. **Conditional on both A7 and E5**, not A7 alone: the UU
   fallback writes any segment at offset 0 (#346) and repeats no Message-ID, so
   A7 cannot see it. With both in place the
   residual population is unknown and presumed small, and #387's
   interval-structure design is deferred until the counter justifies it. **If E5
   is not taken, this decision does not hold** and E3 must be a guard.

4. **The NZB's `bytes` gains no authority.** It stays advisory. D3 observes
   disagreement without acting on it, `offsetOutOfRange` keeps its 12.5% slack
   unchanged, and the slack figure is not re-derived from measurement as part of
   this work.

   **Caveat found in review, and it is a genuine conflict with decision 1.**
   `ExpectedSize` is `Manifest.FileBytes(fileIdx)`, which traces back to the sum
   of the segments that *survived* parsing. A1 already rejects today, so only the
   **newly** rejecting rules — A2–A5 and A7 — shrink the sum. Each one therefore
   shrinks the denominator and **tightens** `offsetOutOfRange`'s limit, and
   separately shrinks the `preallocateFile` size (benign, but unnamed until now)
   — decision 1 silently moving a bound decision 4 promises to leave alone. A
   file that loses one segment to rejection could see a legitimate write to a
   later offset refused. Implementing decision 1 must either keep rejected
   segments' `bytes` in the `ExpectedSize` sum, or state that it does not and
   accept the tightening deliberately. **Do not discover this during
   implementation.**

The through-line: **reject only what could never have worked, warn about
everything else, and let par2 adjudicate.** Rejection is reserved for claims
that are self-defeating — a Message-ID that cannot be fetched, a duplicate that
cannot be placed. Every disagreement where both readings remain possible is
surfaced, not resolved, because resolving it means guessing and guessing
destroys data.

## 9. Rules for anyone extending this

An index, not new material — each rule names the section that argues for it.

- **Ask whether a check is needed at all** (§4). If the *data model* permits the
  violation, change the key or the type. Only reach for the ladder when the
  *world* permits it — and check the substitute type has no valid zero.
- **Classify before you check** (§2, §4). The cost of misfiling a claim as
  unfalsifiable is silent: its bound gets pushed inward and nothing reveals it.
- **A check on a field with no consumer is ceremony** (§5.D). Grep for readers
  first; a warning is not a consumer.
- **Never add a control for something in §6.** If the mechanism cannot succeed
  against the failure it names, it is a comment, not a control.
- **Reject only what could never have worked** (§8). Otherwise warn — even for
  an unambiguous spec violation.
- **A refusal must always produce a recorded disposition** (§7). Silent drops
  are how #392's machinery came to exist.
- **Do not tighten yEnc syntax tolerance** (§3) without evidence from real
  postings. Count first.
- **State the invariant where it is enforced, and again where it is assumed.**
  The whole #386 family came from invariants that were real and unwritten.
