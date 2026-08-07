# nntpfaultproxy

A validation-only tool that sits between gonzbd and a real NNTP provider
and injects faults into chosen articles, so the corruption-detection paths
added for DirectUnpack/quickcheck/repair (RAR) and go_sevenzip (7z) can be
validated against real NZBs and real servers — without needing an NZB
that's naturally incomplete.

**This tool is never built into the production gonzbd binary.** It's a
separate `go build`/`go run` target under `scripts/`, exactly like
`scripts/check_coverage` and `scripts/check_test_alignment`.

## How it works

It's a transparent relay for the small, fixed set of NNTP commands
gonzbd's client actually sends: `CAPABILITIES`, `AUTHINFO USER`/`PASS`,
`BODY`, `STAT`, `QUIT`. Everything passes through untouched except
`BODY`/`STAT` requests matching a configured fault rule.

Three fault actions:

- **`drop`** — responds `430 no such article` immediately, without
  contacting upstream. Matches what a real provider does for a genuinely
  missing article — drives gonzbd's real per-server retry, backup-server,
  and `MaxArtTries` exhaustion logic (`internal/downloader/dispatch.go`).
  This is the closest match to the originally reported bug: articles that
  permanently fail, leaving a "complete" file with gaps.
- **`corrupt`** — relays the real article but flips a configurable number
  of random bytes in the body before forwarding it. Simulates a transport
  that silently delivers wrong bytes. Exercises the RAR/7z CRC32
  verification fixes directly.
- **`timeout`** — accepts the request but never responds, holding the
  connection open for a configurable duration before closing it.
  Exercises gonzbd's idle read-deadline and bad-connection handling.

## Setup

1. Build it:

   ```bash
   go build -o /tmp/nntpfaultproxy ./scripts/nntpfaultproxy
   ```

2. Find the message-IDs you want to target. NZB files store each article's
   message-ID directly in `<segment>` elements:

   ```bash
   grep -o '<segment[^>]*>[^<]*</segment>' your.nzb | head -20
   ```

   Pick a few from the file you expect direct-unpack to handle (e.g. a
   `.rar` or `.7z` volume), and note them without the surrounding
   `<segment ...>`/`</segment>` tags or angle brackets.

3. Write a config, e.g. `/tmp/faultproxy.yaml`:

   ```yaml
   listen: "127.0.0.1:11190"
   upstream:
     host: "news.example.com"   # your real provider
     port: 563
     ssl: true
   rules:
     - message_ids:
         - "abc123@example"     # an article from the .rar/.7z volume you're targeting
       action: drop
   ```

4. Run it:

   ```bash
   /tmp/nntpfaultproxy -config /tmp/faultproxy.yaml
   ```

5. In `gonzbd.yaml`, point one server entry at the proxy instead of the
   real host, keeping the same username/password (forwarded transparently
   via `AUTHINFO`):

   ```yaml
   servers:
     - name: validation
       host: "127.0.0.1"
       port: 11190
       ssl: false              # the proxy terminates plaintext from gonzbd;
                                # it dials the real upstream with TLS itself
       username: "your-real-username"
       password: "your-real-password"
       connections: 4
       priority: 0
   ```

   Use a **separate gonzbd config** (or comment out your other servers)
   for validation runs — don't leave the fault proxy entry in your normal
   production config.

6. Queue the real NZB in gonzbd, watch the logs. Expect to see (for the
   RAR case): the targeted article(s) marked `Failed` after retries are
   exhausted, the file still completing (gap-filled), `directunpack:
   marking set corrupt, volume incomplete`, `quickcheck: ... par2 repair
   will run`, and the `repair` stage actually invoking par2 instead of
   being skipped.

## Validation scenarios

- **Force par2 repair (RAR or 7z):** one `drop` rule on a single article
  from inside a `.rar`/`.7z` volume, with enough other par2 recovery
  volumes available that the job isn't "hopeless" per
  `internal/downloader/dispatch.go`'s `FailedBytes > RecoveryBytes` gate.
- **Force the "needs more recovery blocks" path:** several `drop` rules
  spread across enough articles that `FailedBytes` exceeds the par2 set's
  recovery capacity.
- **Silent corruption (no retry):** a `corrupt` rule on one article inside
  a RAR/7z volume — the article "succeeds" on the wire but the bytes are
  wrong, exercising the CRC32 checks added in rarengine v1.0.3 and
  `go_sevenzip.go`'s `extractSevenZipFile`.
- **Connection health handling:** a `timeout` rule, to confirm gonzbd's
  idle read-deadline fires and the connection is marked bad rather than
  hanging the whole download.

## Percentage-based (rate) faults

For broad fuzzing — "drop roughly 0.1% of all articles" — use `rate`
instead of `message_ids`. A rule with `rate` set and no `message_ids`
matches that fraction of every BODY/STAT request the proxy sees, decided
per request by the seeded RNG:

```yaml
rules:
  - rate: 0.001       # 0.1%
    action: drop
```

A few things worth knowing before relying on this:

- **`rate` is a probability per request, not a guaranteed count.** With
  `rate: 0.001` and 10,000 articles, expect *around* 10 drops, not exactly
  10 — it's a Bernoulli trial per BODY/STAT, not a fixed quota.
- **It applies to every article from every file in the queue**, not just
  the one you're targeting — including par2 index/recovery files, NFOs,
  sample files, anything gonzbd fetches while this server is active. If
  you want the percentage confined to one volume, pair it with a smaller
  `message_ids` list instead, or queue only the NZB you're testing while
  the fault proxy is wired in.
- **Rules are evaluated in order, first match wins.** You can combine a
  precise `message_ids` rule with a broad `rate` rule — the exact-match
  rule takes priority for the IDs it lists, and the rate rule covers
  everything else:

  ```yaml
  rules:
    - message_ids:
        - "abc123@example"   # guaranteed to drop, every run
      action: drop
    - rate: 0.001            # ~0.1% of everything else
      action: drop
  ```

- **Use `-seed <n>` for a reproducible run.** The same seed plus the same
  sequence of accepted connections reproduces the same drop pattern,
  useful for comparing behavior across two runs of the same NZB:

  ```bash
  /tmp/nntpfaultproxy -config /tmp/faultproxy.yaml -seed 42
  ```

- **`rate` works with any action**, not just `drop` — `rate: 0.001` with
  `action: corrupt` silently corrupts ~0.1% of articles instead of
  failing them outright, useful for exercising the CRC32 checks across a
  whole queue rather than one hand-picked article.

## Reproducibility

Pass `-seed <n>` to make `rate`-based rules deterministic across runs (where `n` is a non-zero integer; `-seed 0` or omitting the flag seeds from the current time and is non-deterministic).
`message_ids`-based rules are already fully deterministic (exact match).
