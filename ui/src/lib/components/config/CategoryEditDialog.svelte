<script lang="ts">
	import Modal from '$lib/components/ui/Modal.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import type { CategoryConfig } from '$lib/types';
	import { fetchScripts } from '$lib/api';

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
		pp: 7,
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
					pp: 7,
					script: 'None',
					priority: 0,
					dir: '',
					order: 0
				};
			}
			fetchScripts().then((s) => (scripts = s));
		}
	});

	let repair = $state(true);
	let unpack = $state(true);
	let del = $state(true);

	$effect(() => {
		repair = (draft.pp & 1) !== 0;
		unpack = (draft.pp & 2) !== 0;
		del = (draft.pp & 4) !== 0;
	});

	function updatePP() {
		let val = 0;
		if (repair) val |= 1;
		if (unpack) val |= 2;
		if (del) val |= 4;
		draft.pp = val;
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

		<div class="space-y-2">
			<span class="text-sm font-medium">Post-Processing</span>
			<div class="flex flex-wrap gap-4 rounded-md border p-3">
				<label class="flex items-center gap-2 text-sm cursor-pointer">
					<input type="checkbox" bind:checked={repair} onchange={updatePP} class="rounded border-border" />
					Repair
				</label>
				<label class="flex items-center gap-2 text-sm cursor-pointer">
					<input type="checkbox" bind:checked={unpack} onchange={updatePP} class="rounded border-border" />
					Unpack
				</label>
				<label class="flex items-center gap-2 text-sm cursor-pointer">
					<input type="checkbox" bind:checked={del} onchange={updatePP} class="rounded border-border" />
					Delete
				</label>
			</div>
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
