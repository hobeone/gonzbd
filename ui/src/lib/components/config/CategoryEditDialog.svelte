<script lang="ts">
	import Modal from '#lib/components/ui/Modal.svelte';
	import { Button } from '#lib/components/ui/button/index.js';
	import { Input } from '#lib/components/ui/input/index.js';
	import type { CategoryConfig } from '#lib/types.js';
	import { fetchScripts } from '#lib/api.js';
	import { PP_LEVELS, NEW_CATEGORY_PP, parsePPLevel } from '#lib/pp.js';

	let {
		open = $bindable(false),
		category = null,
		onsave
	}: {
		open?: boolean;
		category?: CategoryConfig | null;
		onsave: (c: CategoryConfig) => void;
	} = $props();

	let draft = $state<CategoryConfig>({
		name: '',
		pp: NEW_CATEGORY_PP,
		script: 'None',
		priority: 0,
		dir: '',
		order: 0
	});

	let scripts = $state<string[]>(['None']);

	const isReservedCategory = $derived(
		category ? (category.name === '*' || category.name === 'Default') : false
	);

	$effect(() => {
		if (open) {
			if (category) {
				draft = { ...category };
			} else {
				draft = {
					name: '',
					pp: NEW_CATEGORY_PP,
					script: 'None',
					priority: 0,
					dir: '',
					order: 0
				};
			}
			fetchScripts().then((s) => (scripts = s));
		}
	});

	function updatePP(e: Event) {
		const level = parsePPLevel((e.target as HTMLSelectElement).value);
		if (level !== null) draft.pp = level;
	}

	function handleSave() {
		if (!draft.name) return;
		onsave(draft);
		open = false;
	}
</script>

<Modal bind:open ariaLabel={category ? "Edit Category" : "Add Category"} class="w-full max-w-md p-6">
	<h2 class="text-lg font-semibold">
		{category ? 'Edit Category' : 'Add Category'}
	</h2>

	<div class="mt-4 space-y-4">
		<div class="space-y-1.5">
			<label for="cat-name" class="text-sm font-medium">Category Name</label>
			<Input id="cat-name" bind:value={draft.name} placeholder="e.g. tv" disabled={isReservedCategory} />
		</div>

		<div class="space-y-1.5">
			<label for="cat-dir" class="text-sm font-medium">Folder / Path</label>
			<Input id="cat-dir" bind:value={draft.dir} placeholder="Relative to complete_dir" />
		</div>

		<div class="space-y-1.5">
			<label for="cat-script" class="text-sm font-medium">Script</label>
			<select id="cat-script" bind:value={draft.script} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
				{#each scripts as s (s)}
					<option value={s}>{s}</option>
				{/each}
			</select>
		</div>

		<div class="space-y-1.5">
			<label for="cat-pp" class="text-sm font-medium">Post-Processing</label>
			<select id="cat-pp" value={String(draft.pp)} onchange={updatePP} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
				{#each PP_LEVELS as l (l.level)}
					<option value={String(l.level)}>{l.description}</option>
				{/each}
			</select>
		</div>

		<div class="space-y-1.5">
			<label for="cat-priority" class="text-sm font-medium">Default Priority</label>
			<select id="cat-priority" bind:value={draft.priority} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
				<option value={-100}>Default</option>
				<option value={2}>Force</option>
				<option value={1}>High</option>
				<option value={0}>Normal</option>
				<option value={-1}>Low</option>
			</select>
		</div>
	</div>

	<div class="mt-6 flex justify-end gap-3">
		<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
		<Button onclick={handleSave} disabled={!draft.name}>Save</Button>
	</div>
</Modal>
