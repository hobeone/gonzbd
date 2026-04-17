<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import type { SorterConfig } from '$lib/types';
	import { fetchCategories } from '$lib/api';

	let {
		open = $bindable(false),
		sorter = null,
		onsave
	}: {
		open?: boolean;
		sorter?: SorterConfig | null;
		onsave: (s: SorterConfig) => void;
	} = $props();

	let draft = $state<SorterConfig>({
		name: '',
		order: 0,
		min_size: 0,
		multipart_label: '',
		sort_string: '',
		sort_cats: [],
		sort_type: [1], // Default to TV
		is_active: true
	});

	let categories = $state<string[]>([]);

	$effect(() => {
		if (open) {
			if (sorter) {
				draft = { ...sorter, sort_cats: [...sorter.sort_cats], sort_type: [...sorter.sort_type] };
			} else {
				draft = {
					name: '',
					order: 0,
					min_size: 0,
					multipart_label: '',
					sort_string: '',
					sort_cats: [],
					sort_type: [1],
					is_active: true
				};
			}
			fetchCategories().then((c) => (categories = c));
		}
	});

	function toggleCat(cat: string) {
		if (draft.sort_cats.includes(cat)) {
			draft.sort_cats = draft.sort_cats.filter((c) => c !== cat);
		} else {
			draft.sort_cats.push(cat);
		}
	}

	function handleSave() {
		if (!draft.name || !draft.sort_string) return;
		onsave(draft);
		open = false;
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-[60] bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-[70] w-full max-w-xl -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-xl overflow-y-auto max-h-[90vh]">
			<Dialog.Title class="text-lg font-semibold">
				{sorter ? 'Edit Sorter' : 'Add Sorter'}
			</Dialog.Title>

			<div class="mt-4 space-y-4">
				<div class="space-y-1.5">
					<label for="sorter-name" class="text-sm font-medium">Sorter Name</label>
					<Input id="sorter-name" bind:value={draft.name} placeholder="e.g. TV Sorter" disabled={!!sorter} />
				</div>

				<div class="space-y-1.5">
					<label for="sorter-template" class="text-sm font-medium">Sort String (Template)</label>
					<Input id="sorter-template" bind:value={draft.sort_string} placeholder="e.g. %fn/%fn.%ext" />
					<p class="text-[10px] text-muted-foreground uppercase font-bold">Use tokens like %sn (Show Name), %s (Season), %en (Episode Name)</p>
				</div>

				<div class="grid grid-cols-2 gap-4">
					<div class="space-y-1.5">
						<label for="sorter-min-size" class="text-sm font-medium">Min Size (MB)</label>
						<Input id="sorter-min-size" type="number" bind:value={draft.min_size} />
					</div>
					<div class="space-y-1.5">
						<label for="sorter-order" class="text-sm font-medium">Execution Order</label>
						<Input id="sorter-order" type="number" bind:value={draft.order} />
					</div>
				</div>

				<div class="space-y-2">
					<span class="text-sm font-medium">Applied Categories</span>
					<div class="flex flex-wrap gap-2 rounded-md border p-3">
						{#each categories as cat}
							<label class="flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs cursor-pointer transition-colors
								{draft.sort_cats.includes(cat) ? 'bg-blue-50 border-blue-200 text-blue-700' : 'bg-gray-50 border-gray-200 text-gray-600 hover:bg-gray-100'}">
								<input
									type="checkbox"
									class="hidden"
									checked={draft.sort_cats.includes(cat)}
									onchange={() => toggleCat(cat)}
								/>
								{cat}
							</label>
						{/each}
						{#if categories.length === 0}
							<span class="text-xs text-gray-400 italic">No categories found</span>
						{/if}
					</div>
				</div>

				<div class="flex items-center gap-2 py-2">
					<input id="sorter-active" type="checkbox" bind:checked={draft.is_active} class="rounded border-gray-300" />
					<label for="sorter-active" class="text-sm font-medium cursor-pointer">Sorter Active</label>
				</div>
			</div>

			<div class="mt-6 flex justify-end gap-3">
				<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
				<Button onclick={handleSave} disabled={!draft.name || !draft.sort_string}>Save</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
