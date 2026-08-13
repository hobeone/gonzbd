# Svelte 5 UI — Known Gotchas

Read this before creating, editing, or refactoring any `.svelte` or
`.svelte.ts` file. Not relevant to Go-only changes.

## Module-level `$state` in `.svelte.ts` files does not reliably trigger re-renders

**Problem**: Reactive state declared with `$state` in `.svelte.ts` module files (stores) does not reliably trigger template re-renders in consuming components when mutated inside `async` functions. Getter functions like `getConfig()` that return `$state` properties work for the initial read but miss subsequent updates. This was discovered when the SettingsDialog showed "Loading configuration..." indefinitely despite the fetch completing successfully.

**Rule**: For any component that fetches data and renders it conditionally (loading → error → data), declare `$state` variables **inside the component**, not in an external `.svelte.ts` store module. Use `.then()` chains rather than `async`/`await` for the fetch to ensure state mutations happen in a context Svelte can track.

**Pattern that works** (used in `SettingsDialog.svelte`):
```svelte
<script lang="ts">
  let data = $state(null);
  let loading = $state(false);

  $effect(() => {
    if (open && !data && !loading) {
      loading = true;
      fetch('/api/...')
        .then(res => res.json())
        .then(d => { data = d; })
        .finally(() => { loading = false; });
    }
  });
</script>
```

**Pattern that does NOT work**:
```typescript
// store.svelte.ts — mutations here don't trigger component re-renders
let data = $state(null);
export async function load() {
  data = await fetchJSON(...); // component won't see this change
}
```

> Note: this gotcha is about **module-level** `$state`. Class-field `$state`
> inside a store class (e.g. `BasePollStore`) works correctly.

## Dialogs: react to the `open` prop, not to open-change callbacks

All dialogs go through `ui/src/lib/components/ui/Modal.svelte`, a thin wrapper
over the native `<dialog>` element. Parents control it with `bind:open`.

To run logic when a dialog opens (resetting a form, fetching categories), watch
the `open` prop with a `$effect` — do **not** reach for an open-change callback:

```svelte
$effect(() => {
    if (open) reset();
});
```

This rule predates `Modal.svelte`: the previous `bits-ui` `Dialog.Root` fired
`onOpenChange` only when the dialog *itself* initiated the change (overlay or
close-button click) and stayed silent when the parent set the bound prop, which
made callback-based logic silently miss parent-driven opens. `Modal.svelte` has
no such callback at all, so the `$effect` pattern is now the only option.

Two things `Modal.svelte` relies on that are easy to break when editing it:

- The `$effect` guards on `!dialogEl.open` before calling `showModal()` —
  calling `showModal()` on an already-open dialog throws.
- Both dismissal paths must write back to the bound prop, or the dialog can
  never be reopened: the native `close` event covers <kbd>Esc</kbd> and
  programmatic `close()`, and a click handler comparing `e.target === dialogEl`
  covers backdrop clicks (clicks on `::backdrop` report the dialog as target).

Closed dialogs are unmounted via `{#if open}` and additionally hidden by
`dialog:not([open]) { display: none !important; }` in `app.css`, because a
mounted-but-closed `<dialog>` still hit-tests and swallows clicks meant for the
page underneath.

## Child component updates

ConfigInput/ConfigSwitch and similar child components should receive an `onupdate` callback prop rather than importing store functions directly. This keeps the data flow explicit and avoids the module-level `$state` reactivity issue.

## Do not run `npx prettier --write` in `ui/`

`ui/` has **no Prettier configuration and no Prettier dependency** — there is no
`.prettierrc`, and `package.json` does not list it. `npx prettier --write .`
therefore downloads Prettier and reformats the entire tree with its defaults,
which do not match the style the files are written in. The result is a
~1400-line diff that touches almost every component and hides the change you
actually made inside it. An implementer has already lost work to this.

If you need formatting, format only the file you edited and check the diff
before staging it. Adding a `.prettierrc` would be a project-wide style
decision, not a local convenience, and needs to be agreed rather than
introduced as a side effect of an unrelated change.
