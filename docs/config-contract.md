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
while it runs. The `--download-dir` flag is applied by `resolveDirs` at startup.

Known limitations:

- Editing `download_dir` in the YAML and restarting while jobs are queued is
  unsupported. The check runs only on a runtime change; at startup the new value
  simply applies.
- A job queued between the check and the swap is not covered.
- Only registered jobs block a change. A failed history entry awaiting retry is
  not registered, so a retry after the change looks under the new base.

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
