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
