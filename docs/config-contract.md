# Config Documentation & UI Contract

Read this before adding, renaming, or removing a field in `internal/config/`,
or a `keyword=` prop on a `ConfigInput`/`ConfigSwitch`/`ConfigTextarea` in
`ui/src/lib/components/config/`. Not relevant to changes that don't touch the
config schema or config UI.

## Config Documentation Sync

The root `gonzbd.yaml` contains inline comments above every directive documenting
its purpose, valid values, and important considerations. When adding, renaming,
or removing config fields in `internal/config/`, you MUST update the
corresponding comments in `gonzbd.yaml` and `test/fixtures/gonzbd.yaml` to stay
in sync. Also update the settings tables in `docs/sabnzbd_spec.md` §9.

## `download_dir` cannot change under a queued job

A job's files are written under the `download_dir` in force when each file is
registered, joined with the job's name, but post-processing re-derives the
directory from the `download_dir` in force at hand-over.
A changed base while a job is registered would therefore point
post-processing, and finalize's moves, at a directory the job never wrote to.

`Application.SetDownloadDir` refuses with `ErrDownloadDirBusy` while the
dispatcher holds a row whose outcome is not settled, and `set_config` reports
that as HTTP 409. Setting the value already in force always succeeds. The API
handler offers the application the new value before it touches the live config
or the file on disk, so a refusal leaves all three on the old value.
Basis for `set_config` being the runtime path that changes the directory:
`git grep -n 'SetDownloadDir(' -- '*.go' ':!*_test.go'` finds 4 lines, of
which one is a call (`internal/api/config.go`) and three are declarations.
`git grep -n 'config\.Load(' -- '*.go' ':!*_test.go'` finds 2 lines, the daemon
at startup (`cmd/gonzbd/main.go`) and a separate tool (`scripts/nzbprobe`), and
the daemon has no SIGHUP or file-watch handler, so nothing reloads the file
while it runs. `git grep -n 'resolveDirs(' -- 'cmd/*.go' ':!*_test.go'` shows
the `--download-dir` flag is handled only at startup, in `cmd/gonzbd`. Its
value is used to create directories and, when `general.admin_dir` is blank, to
derive the admin directory, but it is never written to the config that
`app.New` reads, so the flag does not set the application's download directory.

Known limitations:

- Editing `download_dir` in the YAML and restarting while jobs are queued is
  unsupported. The check runs only on a runtime change; at startup the new value
  simply applies.
- A job added between the check and the swap is not covered, and ends up
  consistent on the new base, because its files take the pipeline's directory
  when each file is registered. A retry in that window is the failing case:
  the failed directory is restored under the old base while registration uses
  the new one, so the job fails with its bytes left under the old base.
- Two concurrent `download_dir` sets can leave the pipeline and the config
  disagreeing, because nothing serializes them.
- If `config.Save` fails after `SetDownloadDir` succeeded, the running daemon
  uses the new directory while the file on disk still holds the old one.
- Only registered jobs block a change. A failed history entry awaiting retry is
  not registered; a retry of an entry whose recorded path is neither directly
  under the current `download_dir` nor inside `complete_dir` is refused with an
  error naming that path.
- The retry check also accepts an entry when the old `download_dir` sat inside
  the current `complete_dir`, or the current `complete_dir` equals the old
  `download_dir`. The retry then restores nothing and fails with its bytes
  under the old path.
- Deleting, with files, a history entry recorded under an earlier base is
  refused by `safeDeleteDir`, which allows only the current `download_dir` and
  `complete_dir`. The handler logs a warning and still removes the entry, so
  those files stay on disk.

## Config ↔ UI Contract Test

`internal/config/ui_contract_test.go` holds `TestUIKeywordsAreValidConfigTags`,
which enforces the config↔UI contract. **It maintains no list.** It walks
`ui/src/lib/components/config/`, extracts every `section=` / `keyword=` pair
from `ConfigInput`, `ConfigSwitch`, `ConfigTextarea` and `ConfigSelect` with a
regex, and asserts each one resolves to a settable Go config tag. So the Svelte
components are the source of truth and the test follows them:

- When you **add a new `keyword=` prop** to any of those four components, nothing needs adding to the test — it discovers the prop on the next run, and fails if the Go field it names does not exist.
- When you **remove or rename a Svelte keyword**, likewise: the test follows the component.
- When you **rename or remove a Go config field** (changing its `json:` tag), `TestAllFlatConfigTagsAreSettable` catches the breakage automatically — but you must also update any matching Svelte `keyword=` props, because that direction is what `TestUIKeywordsAreValidConfigTags` fails on.
- Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'` to verify after any config or UI change.
