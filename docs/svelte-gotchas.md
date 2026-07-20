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

## `bits-ui` `onOpenChange` vs `bind:open`

When a parent component controls a Dialog's open state via `bind:open`, the `onOpenChange` callback on `Dialog.Root` only fires when the dialog *itself* initiates a state change (clicking overlay/close). It does **not** fire when the parent sets the bound prop. Use a `$effect` watching the `open` prop instead.

## Child component updates

ConfigInput/ConfigSwitch and similar child components should receive an `onupdate` callback prop rather than importing store functions directly. This keeps the data flow explicit and avoids the module-level `$state` reactivity issue.
