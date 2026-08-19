# Article Validation Contract

> **Status: in progress; dispositions settled.** F3, A7, F6, F2, F5/B1 and the
> whole A-block have landed; C5, E3–E5 and F1/F4 remain proposed. §8 is no longer open — what each class of violation
> produces has been decided and is binding on the work that follows. This
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

## Ground rules

Two standing constraints that precede every section below. They are not
validation policy; they decide which of the options in §4 is even on the table,
and both have already changed conclusions this document reached without them.

> **These two rules are project-wide, and `AGENTS.md` § "Standing Design Rules"
> is where they are stated canonically.** They were written here first, which
> was a placement error: this document is read only before touching
> `internal/nzb`, `internal/nntp`, `internal/decoder` and the two paths named in
> its header, while the rules govern constructor design and migration decisions
> everywhere. Someone working in `internal/queue` or `internal/history` would
> never have seen them.
>
> What follows is the **argument** — why each rule holds, what it forbids, and
> the worked cases. If the two ever disagree, `AGENTS.md` is the rule and this
> section is out of date.

### No backwards compatibility

GoNZBD targets fresh installations, is explicitly **not** a drop-in replacement
for Python SABnzbd, and runs as a single self-administered instance. Therefore:

> **No change in this document owes anything to state written by an earlier
> version of this code, or to parity with any other implementation.**

Persisted manifests, queue rows, history entries and NZB backups written before
a change may be assumed to satisfy the invariants that change introduces. There
is no drain period, no dual-read path, no migration, and no "old jobs behave
differently" caveat. Where an invariant is newly enforced at ingestion, it is
simply true everywhere.

The rule's real force is in what it forbids, not what it permits:

> **Before writing a guard, state which persisted or foreign state makes it
> necessary — then check whether that state is in scope at all.** If the only
> answer is "data an earlier build wrote", the guard is not needed; delete the
> class instead of defending against it.

This is not a licence to skip validation of things the *world* produces. The
distinction that matters throughout §4 and §5:

| Input | Trusted? | Why |
|---|---|---|
| A parsed NZB | **no** | a third party wrote it |
| An NNTP response | **no** | §1 — untrusted for resource consumption, identity verifiable |
| A manifest we wrote, read back | **yes** | we wrote it, under invariants we enforce |
| A manifest an *older build* wrote | **out of scope**, except below | this rule |
| A manifest corrupted on disk | **yes-with-integrity-check** | a different failure class; `internal/durability` owns it, not this document |

The fourth row is the one this rule eliminates. The fifth is unaffected by it:
truncation and bit-rot are not versioning, and nothing here weakens the
durability contract's checks.

**The one carve-out, and it is narrow: the rule waives persistence FORMAT, not
a security invariant.** Where the state an older build persisted could, if
trusted, hand an attacker something — rather than merely produce a stale
figure or a missing field — the guard stays and the rule does not excuse it.
There is exactly one such case today. `Manifest.UnmarshalJSON` re-applies
`nzb.MessageIDIsFetchable` to every article ID it reads, because
`internal/nntp` no longer validates Message-IDs at all (§5.A) and a manifest
written before that parse-time rule existed can still carry a CR or LF — which
is a command injection, not a formatting difference.

The test for the carve-out is what the trusted value could *do*, not how old
it is:

- A stale `total_bytes`, a missing counter, a figure computed by a superseded
  rule → the rule applies, delete the guard.
- A value that is interpolated into a protocol, a path, a query, or a command
  → the rule does not apply, keep the guard and say why at the check.

This is why §5.A can say "do not add a uniqueness check to `Manifest`
construction" while `UnmarshalJSON` gained a *fetchability* check in the same
release. A duplicate Message-ID resolves a lookup to the wrong article, which
is a correctness bug the ground rule genuinely does dispose of. A Message-ID
carrying CRLF writes attacker-chosen bytes to a socket. Only the second earns
a second enforcement point.

**Two concrete consequences, both settled below.** The Python-parity constraint
on `NZB.MD5`'s digest ordering is void (§8, decision 1), and A7's invariant
holds for restored jobs as well as newly-parsed ones, which unblocked F2 (§5.A,
§5.F).

### State has one owner

The second rule is the one that has retired the most defects in this codebase —
#372 gave every `FileWriter` product a consumer, #378 gave the article
accounting an owner, #385 made offset collisions exact.

> **Every piece of derived state has exactly one function that computes it and
> exactly one path that mutates it. Everything else reads.**

A field whose value is *documented* as a function of other fields, but which any
caller may assign, is not an invariant — it is a comment. The distinction is not
academic: #394 attempted to add dropped-duplicate bytes back into `File.Bytes`
from outside its derivation, which broke the documented identity
`File.Bytes == Σ Articles[].Bytes` and stranded bytes in `JobProgress`'s
`remaining` forever. The comment saying `File.Bytes` was that sum did not stop
it. `normalizeFileStruct` being the sole writer is what makes it true.

Two smells this rule names, both present in the tree today:

- **Two constructors for one type.** `newManifest` and `Manifest.UnmarshalJSON`
  populate the same eight fields by two independently-maintained code paths.
  They have already diverged: `newManifest` *derives* `totalBytes` by summing
  `f.Bytes`, while `UnmarshalJSON` *trusts* `mj.TotalBytes` from disk, and
  nothing reconciles them. `recoveryFigures`' doc comment — "Both construction
  paths … call it, so the two cannot disagree" — is a comment doing an owner's
  job, and it only covers the two fields someone remembered.
- **A derived value that is also persisted.** Anything recomputable from the
  articles (a file's byte total, recovery figures, article offsets) should be
  derived on load, not stored and trusted. Storing it creates a second source of
  truth that can disagree with the first, and under the no-backcompat rule there
  is no reason to keep the stored copy.

The remedy for both is the same and is cheap here: `UnmarshalJSON` decodes to
the public `[]JobFile` shape and delegates to `newManifest`. One derivation, one
owner, and the persisted `TotalBytes` field stops being read at all.

> **When a check and an owner would both work, take the owner.** A check must be
> called at every site that could violate the invariant, and the failure mode of
> forgetting one is silence. An owner cannot be forgotten, because there is
> nowhere else to write.

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
| Message-ID of the served article | our request, the success response line | **Ours.** Fully decidable. |
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

## 4. The placement rule — and the two tiers above it

Before applying the ladder, ask whether the ladder is needed at all. There are
three tiers, in order of preference, and the ladder is the last of them.

> **Tier 1 — make the violation unrepresentable.** Change the key or the type so
> the bad state has no encoding. Costs nothing at runtime and cannot be
> bypassed.
>
> **Tier 2 — give the state an owner.** When the type cannot exclude the bad
> value, make one function the sole place it is computed and one path the sole
> place it is mutated. The invariant then holds because there is nowhere else to
> write it, not because every writer remembered a rule.
>
> **Tier 3 — enforce on the ladder.** A check, at the outermost layer that can
> decide it. Costs a check, a test, and an error path — three things that can be
> wrong, and one that can be forgotten at a call site.

Tier 2 is the one this codebase reaches for least and has gained most from
(Ground rules, above). It is also the answer whenever tier 1 *almost* works: a
type that cannot be made incapable of the bad value can still be made
unreachable except through a gatekeeper.

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

**Tier 1 has a precondition, and F1 sits right on its edge.** It works only
when the replacement identifier has **no representable-but-invalid value**.
`""` is not a valid Message-ID, so an empty-string key is a loud, checkable
error. `0` *is* a valid `ArtIdx`. Re-keying therefore trades a noisy guard for a
silent alias: a struct-zero `WriteRequest` reaching the accept path would
address article 0 rather than trip anything. Today that is excluded by the
`FileIdx` sentinel check, **not by the type**.

**This is exactly the case tier 2 exists for, and it is cheaper than the F4
route the earlier draft proposed.** `Assembler.WriteArticle` is already the sole
entry point for an article write from outside `internal/assembler` — both
external construction sites are in `internal/app/pipeline.go`, and the `reqs`
channel is unexported. Making that one function the owner — taking the identity
as explicit parameters, or rejecting a request whose `ArtIdx` was never set —
makes the struct-zero unreachable on the path that matters, without splitting
control messages out of the type first. F4 remains worth doing for its own
reasons (§5.F); it is not a prerequisite for F1.

**The owner has landed** (#402): the signature is
`WriteArticle(ctx, ref ArticleRef, req WriteRequest)`, and `ArticleRef` carries
`JobID`, `FileIdx`, `ArtIdx` and `MessageID`. Omitting the identity is a compile
error at both `internal/app` call sites. The identity fields stay on
`WriteRequest` — the control-message convention builds `WriteRequest` values
carrying `FileIdx` sentinels, and removing them is F4's job, not this one's.

Note what that does and does not buy, because the difference is the whole reason
this section exists. The identity is now **un-omittable**, not
**unrepresentable**: a caller passing `ArtIdx: 0` deliberately is still
indistinguishable from one who never thought about it. What tier 2 removes is
the caller who supplied no identity at all.

**Closing the remainder is rejected, not deferred**, and the reason is worth
stating so it is not queued as work. Article 0 is a legitimate index, so making
the zero invalid means a 1-based encoding — which puts an encode/decode step
between the manifest's numbering and the assembler's, and creates a second
representation of the same number. That is a broad, invisible owner violation
traded for a narrow, visible one, and it is the worse deal before and after F1.

The residual risk is also smaller than the bare statement suggests. Under the
old signature an omitted `ArtIdx` hid among `Offset`, `Data` and `FatalErr` in a
payload literal. It would now have to be omitted from a struct whose only
purpose is identity, with the other three fields filled in beside it — a
self-announcing mistake rather than a quiet one.

The general form: **where the substitute type has a valid zero, tier 1 converts
a detectable failure into an undetectable one — so pair it with tier 2 rather
than abandoning it.**

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
| A1 | Message-ID non-empty | L0 | ✅ **implemented** — reject, counted | — |
| A2a | Message-ID ≤ 495 octets (NNTP argument bound) | L0 | ✅ **implemented** — reject, counted | — |
| A2b | Message-ID ≤ 248 octets (RFC 3977 §3.6, bare form) | L0 | ✅ **implemented** — count only | evidence |
| A3 | Message-ID contains no SP, HT, CR, LF or NUL | L0 | ✅ **implemented** — reject, counted | — |
| A4 | wrapper normalised away; no *interior* `<` or `>` | L0 | ✅ **implemented** — reject, counted | — |
| A5 | Message-ID is printable US-ASCII | L0 | ✅ **implemented** — count only | evidence |
| A6 | Message-ID contains `@` | L0 | ✅ **implemented** — count only | evidence |
| A7 | Message-ID unique job-wide | L0 | ✅ **implemented** — later segment dropped, counted, warned at ingest | — |
| B1 | a success response's Message-ID equals the requested one (`BODY` 222, `ARTICLE` 220, `HEAD` 221, `STAT` 223) | L1 | ✅ **implemented** — drop the connection; the article fails with it | — (built as F5) |
| C1–C3 | payload self-verification | L2 | ✅ already enforced | — |
| C4 | checksum presence | L2 | count only | — |
| C5 | `offset + len ≤ size`; `end − begin + 1 == len` | L2 | fail article (`end=` half counts first) | — |
| D1–D3 | NZB ↔ article disagreements | L3 | job-level warning | — |
| E1–E2 | bounds, exact-offset collision | L4 | ✅ already enforced | — |
| E3 | range overlap | L4 | **telemetry only** | A7 **and** E5 |
| E4 | part tiling / gaps | L0 + L4 | warn at ingestion | — |
| E5 | UU body only satisfies a single-segment file | L3 | reject | — |
| F1–F5 | structural (§5.F) | — | nothing — the state stops existing | — |
| F6 | digest over accepted IDs (§5.F) | — | ✅ **implemented** | — |

**Build order.** The F-items land first (§5.F), then the assertions:

> ~~F3~~ → ~~A7~~ → ~~F6~~ → ~~A1–A6~~ → ~~F2~~ → ~~F5/B1~~ → C5 → E5 → F1a (fixture `ArtIdx`) → F1b (flip the key) → E3/E4

Struck items have landed. F3 led because it was the smallest instance of the
pattern and the cheapest place to learn its cost; doing it first is what
established that the two-commit split in §5.F applies to the whole class rather
than to F1 alone.

**The order changed once the ground rules were applied**, and the reasons are
worth keeping because they are the rules doing work rather than a preference:

- **F6 and A1–A6 moved to the front, together.** F6 deletes the digest-ordering
  hazard (§8, decision 1). Every rejection that `continue`s out of the segment
  loop had to be placed after the `digest.Write` or it silently changed the
  document's identity, and the A-block adds three such rejections (A2a, A3, A4)
  on top of relocating A1's. With F6 in place the A-block is a mechanical
  addition to one function; before it, it was the riskiest work in the document.
  Neither half is worth doing without the other: F6 alone deletes a trap nothing
  is currently walking into, and A1–A6 alone walks into it. The counted rows
  (A2b, A5, A6) never interact with the hazard, since a counted segment is still
  accepted and hashed.
- **F2 moved earlier and got cheaper.** Its remaining blocker was that a job
  persisted before A7 could still hold duplicate Message-IDs on restore, so
  `messageIDIndex` had to keep working for those. The no-backcompat rule deletes
  that case, leaving F2 a pure deletion with no compatibility path.
- **F1 moved later.** It is the most expensive item (~80 fixture literals) and
  the only one whose value is a code deletion rather than a closed defect. It is
  also no longer entangled with F4 — §4's tier-2 argument closes its valid-zero
  problem at `WriteArticle` instead.

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

**Conformance is not the criterion — fetchability is.** The rows below split on
a single question, and it is deliberately not "does this violate the RFC":

> **Write the request this ID produces. Does it name an article a server could
> answer?**
>
> If not, the segment could never have downloaded and rejecting it costs
> nothing. If it could, rejecting it fails an article that works today, on no
> evidence, and the correct disposition is a counter.

The concrete form of that question is `BODY <%s>\r\n` — the line the ID is
interpolated into. Applying it to all six rows does **not** group them the way
RFC conformance does, and the earlier draft of this section grouped them wrong:
it had A6 alone as the count-only exception, because A6 is where the criterion
was first noticed rather than where it stops applying.

| # | On the wire | Fetchable? | Disposition |
|---|---|---|---|
| A1 | `BODY <>` | **no** — names nothing | reject |
| A2a | line over the NNTP limit | **no** — the server rejects the *line* | reject |
| A2b | `BODY <251..495 octets>` | **yes** — nonconformant, syntactically fine | count |
| A3 | `BODY <a b>` | **no** — breaks the command grammar | reject |
| A4 | `BODY <a>b>` | **no** — closes the wrapper early | reject |
| A5 | `BODY <caf\xc3\xa9@h>` | **yes** — the server's call | count |
| A6 | `BODY <noat>` | **yes** | count |

| # | Assertion | Status |
|---|---|---|
| A1 | Message-ID is non-empty | ✅ enforced and counted (`EmptyMessageIDs`) |
| A2a | ≤ 495 octets, the NNTP argument bound | ✅ enforced (`OversizeMessageIDs`) |
| A2b | ≤ 248 octets | ✅ counted (`NonConformantMessageIDs`), still dispatched |
| A3 | no whitespace (SP, HT), no CR/LF/NUL | ✅ enforced at L0 (`MalformedMessageIDs`); the L1 injection guard is gone, this replaces it |
| A4 | no `<` or `>` inside | ✅ enforced — a matched wrapper is normalised away first, so `Article.ID` is always bare |
| A5 | printable US-ASCII only | ✅ counted (`NonASCIIMessageIDs`), still dispatched |
| A6 | contains `@` | ✅ counted (`MessageIDsMissingAtSign`), still dispatched |
| A7 | unique across **every segment of the whole NZB**, not merely within one file | ✅ **enforced** — the later segment is dropped and counted in `DuplicateMessageIDs` |

**A2 splits, and the RFC's 250 is the wrong number to reject on.** RFC 3977
§3.6's 250-octet limit is a conformance bound on the identifier; §3.1's limit is
a protocol bound on the *line*, and only the second one makes an article
unfetchable. §3.1 caps a command line at 512 octets including CRLF and its
arguments at 497, so for `BODY <id>\r\n` the binding constraint is the argument
`<id>` — giving roughly **495 octets** of Message-ID. Pin the exact constant
against the RFC text when implementing rather than inheriting it from this
sentence; the point is which clause binds, not the arithmetic.

Between 251 and that bound the ID is nonconformant and perfectly requestable,
so it counts. Above it the server rejects the line rather than the article,
which is a different and worse failure — that half rejects.

**A7 is enforced.** `partitionSegments` carries a document-wide seen-set and
drops any segment whose Message-ID an accepted segment already claimed.
Downstream code may therefore treat Message-ID as a unique key within a job.
F2 removed the last by-ID lookup **in `internal/queue`**, but the guarantee is
still load-bearing elsewhere: `internal/assembler`'s `FileWriter.seenDone` and
`.seenFailed` remain keyed on Message-ID until F1 re-keys them on `ArtIdx`, and
two articles sharing an ID would make the second look like a duplicate of the
first — dropped, with the assembled file silently short. **A7 is not
retire-able until F1 lands.** The drop once had
to sit after the `digest.Write` to leave `NZB.MD5` unchanged; F6 removed that
constraint, and the placement is now free.

**A1, A2a, A3 and A4 are rejected at L0** with a counter, exactly as
`BadArticles` already works — the segment does not become an `Article` and is
never dispatched. All four make the wire request itself malformed, so they could
not have produced a successful fetch under any server.

**A2b, A5 and A6 are counted and dispatched.** They are RFC violations that
leave a requestable identifier, so rejecting them would fail articles that
download today. Their counters share the reporting path A7 already built —
`articleCounters` → `parseAnomalySummary` → `job.Warning` — so the cost of
counting is a field, not a mechanism.

**A1 stays, but it is no longer load-bearing.** It is still worth converting the
silent drop into a counted rejection, because a silent drop is how #392's
machinery came to exist (§7). But F1 deletes that machinery structurally, so A1
is now hygiene rather than a prerequisite — do not sequence F1 behind it.

**A1's placement in the loop no longer matters.** It once sat before the
`digest.Write`, and moving it was a silent way to change `NZB.MD5`; with the
digest covering accepted articles only, before and after are the same digest.

**A7 is job-wide, not per-file**, because two structures downstream key on
Message-ID with no file scoping and one with no job scoping at all:

- `Manifest.messageIDIndex` (`internal/queue/manifest.go`) was a **job-wide**
  `map[string]int` built last-writer-wins, until F2 deleted it. One Message-ID appearing in two
  *files* of one NZB made `MarkArticleEmitted` / `ClearArticleEmitted` act on
  the wrong article. Per-file uniqueness would not have closed this, which is
  why A7 is document-wide.
- `dispatchTracker.tryList` and `.inFlight` (`internal/downloader/tracker.go`)
  were keyed on Message-ID **alone**, with no job scoping. Two resident jobs
  sharing a Message-ID shared one try-list, so one job's article could exhaust
  its servers without ever having been fetched for the other.

A7 closed the first: with the ID unique across the document, the index could not
resolve to the wrong article — and F2 then removed the index altogether, so the
class is gone twice over. The second is out of A7's
reach entirely — two jobs, not one NZB, each internally valid — and is closed
structurally by F3, which keys the tracker on `(jobID, artIdx)`.

**A7 holds for restored jobs too, and this is a ground-rule consequence rather
than an enforcement.** `Manifest.UnmarshalJSON` replays stored article IDs
verbatim and re-checks nothing, so an earlier draft recorded that the invariant
was "real for new jobs and untrue for old ones until the queue drains", and kept
`messageIDIndex` working for the latter. The no-backcompat rule deletes that
population outright: every manifest on disk was written by a parser that
enforces A7. **Do not add a uniqueness check to `Manifest` construction** — it
would guard a state that no longer occurs, on a structure F2 has deleted. (The
*fetchability* check `UnmarshalJSON` does carry is a different matter, and the
Ground rules' security carve-out states why: a duplicate ID is a correctness
bug the no-backcompat rule disposes of, a CRLF-bearing ID is an injection it
does not.)

That said, the *reason* the invariant survives a round-trip is worth stating,
because it is tier 2 and not luck: the manifest's articles are written by the
same parser that enforces A7, and nothing between there and disk may add one.
Preserving that is what makes `newManifest` the right sole owner (Ground rules)
— an `UnmarshalJSON` that builds the manifest by its own separate path is one
edit away from being able to violate an invariant its sibling constructor
cannot.

**The two remedies are complementary, not alternatives, and the split is the
general lesson.** Ingestion can enforce uniqueness only inside the document it
is parsing; anything whose scope is wider than one NZB has to change its key
instead. So: **a Message-ID-keyed structure whose scope exceeds one job is a bug
waiting for a duplicate, and ingestion cannot save it.**

F2 kept its value after A7, but for a different reason than it was filed with.
Its correctness rationale — `messageIDIndex` resolving to the wrong article —
was gone. What remained was a `map[string]int` sized `NumArticles()` per
resident job that no longer needed to exist, so it was re-argued on memory
rather than correctness; the ground rules had already turned it from a change
needing a compatibility path into a straight deletion, which was most of its
cost.

**That re-argument came back with more than it was sent for, and the surplus is
the part worth keeping.** Memory turned out to be the *weakest* of three
reasons. The map cost 35–58 B per article, about 0.87 MB for a 20,000-article
job — real, but not decisive on its own. The two that mattered more were
structural. `messageIDIndex` was the sole exception to `Manifest`'s
immutability, so the type's share-by-reference safety argument carried a
carve-out every reader had to re-check; deleting it made that claim
unconditional. And the field's own doc had gone stale in two independent ways —
it named four article-mutation callers when two remained, and asserted they all
held `q.mu` for write when `hydrateSnapshot` held no lock at all. That was
harmless, being a private clone, but it is the shape of comment that stops
being harmless later.

The general lesson, and the reason this is recorded rather than deleted with
the change: **when a rule tells you to re-argue an item on one axis, the
re-argument is also the moment to notice the axes nobody listed.** The
instruction above named memory because memory was what survived A7. It was not
wrong, but it was the smallest of the three things F2 turned out to buy.

**A5 and A6 are counted for the same reason, and the reference implementation
is the evidence.** Python SABnzbd validates a Message-ID nowhere: `nzbparser.py`
checks only the segment's `bytes` and `number` attributes, and `newswrapper.py`
builds the request as `utob("BODY <%s>\r\n" % article.article)` — a UTF-8
encode straight onto the wire. **Non-ASCII Message-IDs have therefore been sent
to real servers by the reference client for years.** Had that been common *and*
rejected by servers, users would have seen unexplained article failures, and
that symptom would plausibly have produced a fix. No such code exists. This is
weak evidence, but it is the only evidence available, and it points away from
rejecting.

**What SABnzbd's silence is not evidence for.** Its non-validation is an
absence, not a decision: the same interpolation means a Message-ID containing
CRLF injects arbitrary NNTP commands from a hostile NZB, which this codebase
refuses and the reference does not. "The reference does not check" bounds how
often a shape occurs in the wild; it says nothing about whether accepting it is
safe. Read it only for the first.

**A3's CR/LF arm is load-bearing, and that was confirmed rather than assumed.**
XML rejects NUL and the other C0 controls outright ("illegal character code"),
so those cannot reach the parser at all — but `&#13;` and `&#10;` decode to a
bare CR and LF, and a literal CR is folded to LF by XML line-ending
normalisation. The injection vector is real and reachable, and after A3 landed
this is the only layer that closes it.

Count first; promote any of A2b/A5/A6 to rejection once its counter says the
case is real and rare.

### B. Response identity (L1 — NNTP)

| # | Assertion | Status |
|---|---|---|
| B1 | a success response's Message-ID equals the requested one (`BODY` 222, `ARTICLE` 220, `HEAD` 221, `STAT` 223) | ✅ enforced (built as F5) |
| B2 | body size ≤ 10 MB | ✅ enforced |

**B1 was the most important gap in the system**, and it is the assumption every
layer from L2 outward silently depends on. `internal/nntp/pipeline.go` matched
responses to requests by **FIFO order only**: the `222 <n> <message-id>` line
was parsed into `cmdResult.line` and never compared. A single desync — one
unsolicited response, one server that answers out of order — silently
mis-attributes *every subsequent article on that connection*, writing correct,
CRC-valid bytes into the wrong file at the offset their own header declares.
Nothing downstream can detect this, because each article is internally
consistent.

**It was built as F5 (§5.F) rather than as an assertion.** Matching was only
needed because `runReader` popped the FIFO blind; the requested Message-ID now
rides on `pendingCmd` and is compared there.

**On the responses that carry an identity, and only those.** An error response
— `430`/`423`, the *common* outcome for a missing article — names no
Message-ID, so it still pairs by FIFO position alone, and a desync landing on a
run of those is undetected until the next success line. The claim is therefore
"a successful response is established as answering the command it was paired
with", not "the pairing is established": stating the second would license a
later change to treat FIFO position as verified everywhere.

`popPending() == nil` had always caught an unsolicited response arriving on an
**empty** FIFO. The live gap was specifically pipelining depth > 1 — the
configuration the shipped sample specifies, `pipelining_requests: 2`, so this
was reachable as configured rather than only under tuning. There is no code
default: `internal/config` requires a positive value, and `newDialOptions`
clamps a zero to 1 only for a config that never went through validation.

Four details the implementation settled, each of which would have been wrong if
guessed:

- The `222` line arrives as one undivided remainder string, so this is
  field-splitting plus angle-bracket normalisation, not a bare string compare.
  The comparison is octet-exact, because RFC 3977 §3.6 makes Message-IDs
  case-sensitive.
- The ID is found **by its wrapper, not by its position**. RFC 3977 §6.2
  brackets it in every response that carries one, so the wrapper locates it
  wherever it sits; reading field 2 positionally assumes the article-number
  field is present, and against a server that omits it yields some other token
  entirely — which then fails the comparison and kills the connection. The
  positional reading fails silently wrong *and* fatally, which is the worst
  available combination.
- The check covers every command kind whose success line echoes a Message-ID
  (`BODY` 222, `ARTICLE` 220, `HEAD` 221, `STAT` 223), not `BODY` alone, and
  reads that set from `cmdKind.successResponse` — one owner for the table, so a
  code cannot be listed for the body decision and forgotten for this one. The
  reader publishes its verdict as `cmdResult.success` so `Fetch` and `Stat` do
  not restate the same codes as literals; two expressions of "this succeeded"
  can drift, and the drift is silent in the direction that matters.
- A success line carrying **no** Message-ID is fatal too, but is reported apart
  from a mismatch. One says the server answered about the wrong article; the
  other says it sent a line this command cannot be matched against at all.
  Both wrap `nntp.ErrDesynced`, because a desync is the one connection failure
  an operator must act on and it is otherwise indistinguishable from a flaky
  link.

**A mismatch drops the connection, it does not merely fail the article.** A
disagreement is proof the FIFO is desynced, which means the *next* response is
mis-paired too. Failing one article and continuing would catch the first
casualty and silently produce all the rest. `finishReader` already existed for
exactly this and is the exit taken.

That half needs a body-less response to pin: with a `222` the reader must
consume a body, so *any* early exit from the mismatch branch leaves that body to
be read as the next status line and the connection dies of a parse error
regardless. `STAT`'s `223` carries no body, so the stream stays parseable across
a mismatch and the drop becomes observable on its own — which is what
`TestStatMessageIDMismatchDropsConnection` exists for.

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

These are changes of representation that delete the states the rest of this
document would otherwise have to guard. For F1–F4, F6 and F7 that is the whole
story: nothing checks them, nothing can fail them, and they need no error path,
because the state they would report has stopped existing.

**F5 is the exception, and it is worth stating rather than glossing.** The
server is outside the program, so no representation choice can make a lying
`222` unrepresentable; F5 is what makes the disagreement *decidable* — it
carries the request's identity to the point the response is read, where
previously the two never met. The check that follows is real and has a real
error path. What F5 deletes is not the check but the ambiguity: without it there
is no fact at that point to compare against, and B1 could only have been an
assertion bolted on somewhere further out, working from data the reader had
already thrown away.

| # | Change | Deletes |
|---|---|---|
| F1 | key `FileWriter.seenDone`/`seenFailed` on `ArtIdx int32`, not `msgID string` | `resolvedUntracked`, `giveBackUntrackedPart`, the `resolvedUntracked` consult in `handleSuccessArticle`, and every `msgID == ""` early return in `fail` / `failPermanent` / `failDisplaced` — i.e. #392 |
| F2 | ✅ **done** — call the by-index `MarkArticleEmittedByIdx` / `ClearArticleEmittedByIdx` everywhere | `Queue.MarkArticleEmitted`, `Queue.ClearArticleEmitted`, `Manifest.articleIndexByID`, `buildMessageIDIndex`, `dropMessageIDIndex`, the `messageIDIndex` field, three eager build sites |
| F3 | key `dispatchTracker.tryList`/`.inFlight` on `(jobID, artIdx)`, not bare `messageID` | three `//nolint:unparam` directives, and the cross-job try-list aliasing bug |
| F4 | split control messages out of `WriteRequest` | the `FileIdx` sentinels, the `JobID == ""` discrimination, the "control message convention" comment class |
| F5 | ✅ **done** — carry the requested Message-ID on `pendingCmd` and match it in `runReader` | makes B1 structural rather than an assertion — see §5.B |
| F6 | ✅ **done** — fold `NZB.MD5`'s digest over **accepted** article IDs, at the acceptance point | the digest-ordering rule, A1's carve-out, and the whole class "a new rejection silently changes every document's identity" — see §8, decision 1 |
| F7 | ✅ **done** — `Manifest.UnmarshalJSON` delegates to `newManifest` | the second constructor, and the persisted `total_bytes` as a source of truth. Landed with the A-block because deleting `validateMessageID` rests on every in-process Manifest deriving its state from one owner |

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
removes control messages, not unset article ones. Two things do: the two-commit
split above, which is why it is mandatory rather than stylistic, and the tier-2
move in §4 — making `Assembler.WriteArticle` the owner of article identity, so a
never-set `ArtIdx` cannot reach the accept path from outside the package at all.
Take the second and F1 stops depending on F4.

Both landed in #402, ahead of F1: every `WriteRequest` fixture carries an
explicit `ArtIdx` under the unchanged Message-ID key, and `WriteArticle` takes
an `ArticleRef`. F1 is therefore a behaviour change against a prepared tree.
Neither closes the valid-zero gap itself, and §4 explains why closing it is
**rejected rather than pending** — a 1-based encoding would buy
unrepresentability at the price of a second representation of every index.

F6 is the exception to this section's framing: it deletes no state, and instead
deletes a **rule about where code may be placed**. It belongs here because the
failure it prevents has the same shape — an invariant maintained by every author
remembering it, with silence as the failure mode — and because the remedy is
likewise structural rather than a check.

None of the six alters behaviour for well-formed input, none needs an L0 change
first, and only F6 interacts with an §8 decision (it settles one). **They land
before the assertions**, because every assertion written first would guard
states these make unreachable — and in F6's case, would have to be placed
correctly by hand five times over.

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

1. **Reject only the unfetchable; count the rest.** A1, A2a, A3, A4 and A7 are
   refused at L0: the segment never becomes an `Article` and is never
   dispatched, with a counter on the parse result alongside `BadArticles`.
   Rejection is cheap for these because they could not have produced a usable
   article under any server — the request they generate is malformed, not merely
   nonconformant.

   **A2b, A5 and A6 are counted, not rejected.** Each leaves a requestable
   identifier, so rejecting would fail articles that download today on no
   evidence. **The criterion is fetchability, not RFC conformance** — write the
   `BODY <%s>\r\n` line the ID produces and ask whether a server could answer
   it (§5.A has the row-by-row table). An earlier draft of this decision had A6
   as the single exception and rejected A2 and A5; that grouping followed
   conformance, applied the fetchability test only to the row where it was first
   noticed, and would have failed working downloads. **A7 is scoped job-wide,
   not per-file**: the downstream structures that a duplicate Message-ID
   corrupts are job-wide.

   **The digest-ordering hazard this decision used to carry has been deleted,
   not documented.** `partitionSegments` folds every structurally-valid article
   ID into the MD5 digest *before* it applies the dedup and size rejections, so
   every new rejection had to be placed after the `digest.Write` — with A1 as a
   carve-out that had to stay before it — or `NZB.MD5` would change for every
   affected document. Nothing failed loudly if that was got wrong, which made it
   the single most dangerous property in the parser for exactly the work §5.A
   proposes: five new rejections, each of which had to be placed correctly by
   hand.

   That ordering existed for one reason: byte-for-byte parity with Python
   SABnzbd's digest, so an NZB imported by either implementation produced the
   same duplicate-job key. **Under the no-backcompat ground rule that goal does
   not exist** — GoNZBD is not a drop-in replacement, and the only two consumers
   of `NZB.MD5` are duplicate detection against our own active queue
   (`ExistsByMD5`) and our own history (`history.SearchOptions.MD5Sum`), both of
   which are self-consistent under any stable definition.

   **Settled and done: the digest covers accepted articles only.** The
   `digest.Write` now sits at the acceptance point, beside
   `seenIDs[id] = struct{}{}`. This was F6 (§5.F) and it landed *before* the
   A-block, which is what made the A-block a mechanical addition rather than
   several chances to silently change every document's identity. It deleted
   the ordering rule, the A1 carve-out, the `//nolint:gosec` rationale's
   parity clause, and ~25 lines of comment whose only job was to stop a future
   reader from moving a line.

   The one-time cost, stated rather than discovered: history rows written before
   the change carry the old digest, so re-adding one of those NZBs will not be
   recognised as a duplicate the first time. It is recognised thereafter, no
   data is lost, and per the ground rule this is not a reason to keep the
   hazard.

   A1's placement no longer matters — before or after the digest write is the
   same digest — and its silent drop is now a counted rejection.

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
   **newly** rejecting rules — A2a, A3, A4 and A7 — shrink the sum; the counted
   rows (A2b, A5, A6) leave it untouched, since their segments are still
   accepted. Each rejection therefore
   shrinks the denominator and **tightens** `offsetOutOfRange`'s limit, and
   separately shrinks the `preallocateFile` size (benign, but unnamed until now)
   — decision 1 silently moving a bound decision 4 promises to leave alone. A
   file that loses one segment to rejection could see a legitimate write to a
   later offset refused. Implementing decision 1 must either keep rejected
   segments' `bytes` in the `ExpectedSize` sum, or state that it does not and
   accept the tightening deliberately. **Do not discover this during
   implementation.**

   **Settled for A7, and by the owner rule for A2a/A3/A4 as well: the tightening
   is accepted.** `normalizeFileStruct` is the sole writer of `File.Bytes` and
   derives it from the articles that survived — so every rejection this document
   adds shrinks the sum by construction, and there is no per-rule decision left
   to make. Re-opening it for the A-block would mean writing `File.Bytes` from
   outside its owner, which is the exact change #394 reverted.

   The fetchability split shrinks this caveat's reach as a side effect worth
   noting: of the five rules that once carried it, two (A2b, A5) no longer
   reject and so no longer move the bound at all.

   That revert is the evidence. Keeping a dropped segment's bytes was tried:
   `File.Bytes` is the sum of `Articles[].Bytes` and is what
   `JobProgress.sizeFigures` derives both expected *and remaining* bytes from,
   so bytes belonging to an article that is not in the manifest can never be
   downloaded and can never be failed, and stay stranded in `remaining` for as
   long as the file is incomplete. The write-bound cost is real but narrow: with the ~2% encoded
   overhead `ExpectedSize` already carries, a rejection needs `k/N` above
   ~12.85%, i.e. one duplicate in a file of seven segments or fewer, and such a
   file is already bound for par2. Distorting every affected job's size
   accounting to avoid it is the worse trade.

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
- **Name the state a guard defends against, then check it is in scope** (Ground
  rules). If the answer is "something an earlier build wrote", there is no such
  state; delete the class rather than defend it. This does not extend to input
  from the world — an NZB or an NNTP response is untrusted regardless.
- **Prefer an owner to a check** (Ground rules, §4 tier 2). A check must be
  called everywhere the invariant could be violated, and forgetting one is
  silent. An owner cannot be forgotten. Two constructors for one type, or a
  derived value that is also persisted, are the two smells.
- **A documented identity between fields is not an invariant unless one function
  owns it** (Ground rules). `File.Bytes == Σ Articles[].Bytes` held because
  `normalizeFileStruct` is its sole writer, not because a comment said so — and
  the one change that wrote it from elsewhere broke it.
- **Classify before you check** (§2, §4). The cost of misfiling a claim as
  unfalsifiable is silent: its bound gets pushed inward and nothing reveals it.
- **A check on a field with no consumer is ceremony** (§5.D). Grep for readers
  first; a warning is not a consumer.
- **Never add a control for something in §6.** If the mechanism cannot succeed
  against the failure it names, it is a comment, not a control.
- **Reject only what could never have worked** (§8). Otherwise warn — even for
  an unambiguous spec violation. **The test is fetchability, not conformance**:
  write the wire request the value produces and ask whether a server could
  answer it. Conformance and fetchability group the Message-ID rules
  differently, and §5.A grouped them by the wrong one until the request line was
  written out.
- **Apply a new criterion to every sibling row, not just the row that prompted
  it** (§5.A). A6's count-only disposition rested on a rule that also covered
  A2b and A5, and the other two rows were written as rejections in the same
  draft; nothing surfaced the contradiction, because a criterion stated in prose
  is not re-run against the rows it was not written for.
- **The reference implementation bounds frequency, not safety** (§5.A). That
  Python SABnzbd omits a check tells you the shape is survivable in the wild; it
  does not tell you accepting it is safe, because SABnzbd never rejects anything
  and lacks guards this codebase has.
- **A refusal must always produce a recorded disposition** (§7). Silent drops
  are how #392's machinery came to exist.
- **Do not tighten yEnc syntax tolerance** (§3) without evidence from real
  postings. Count first.
- **State the invariant where it is enforced, and again where it is assumed.**
  The whole #386 family came from invariants that were real and unwritten.
