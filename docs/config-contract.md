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
in sync. Also update `docs/sabnzbd_spec.md` §9.x tables.

## Config ↔ UI Contract Test

`internal/config/ui_contract_test.go` contains `TestUIKeywordsAreValidConfigTags`,
which is the canonical list of every `keyword=` prop used in Svelte config
components. **This file must be kept in sync with both the Go config structs and
the Svelte UI.** Specifically:

- When you **add a new `keyword=` prop** to any `ConfigInput`, `ConfigSwitch`, or `ConfigTextarea` in `ui/src/lib/components/config/`, add a matching entry to `uiKeywords` in `ui_contract_test.go`.
- When you **remove or rename a Svelte keyword**, remove or update the corresponding entry.
- When you **rename or remove a Go config field** (changing its `json:` tag), the test `TestAllFlatConfigTagsAreSettable` will catch the breakage automatically — but you must also update any matching Svelte `keyword=` props.
- Run `go test ./internal/config/ -run 'TestUI|TestAllFlat'` to verify after any config or UI change.
