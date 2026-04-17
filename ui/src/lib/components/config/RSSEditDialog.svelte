<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Separator } from '$lib/components/ui/separator';
	import type { RSSFeedConfig, RSSFilterConfig } from '$lib/types';
	import { fetchCategories, fetchScripts } from '$lib/api';

	let {
		open = $bindable(false),
		feed = null,
		onsave
	}: {
		open?: boolean;
		feed?: RSSFeedConfig | null;
		onsave: (f: RSSFeedConfig) => void;
	} = $props();

	let draft = $state<RSSFeedConfig>({
		name: '',
		uri: '',
		cat: '',
		pp: '',
		script: '',
		enable: true,
		priority: -100,
		filters: []
	});

	let categories = $state<string[]>([]);
	let scripts = $state<string[]>(['None']);

	$effect(() => {
		if (open) {
			if (feed) {
				draft = JSON.parse(JSON.stringify(feed)); // Deep copy
			} else {
				draft = {
					name: '',
					uri: '',
					cat: '',
					pp: '',
					script: '',
					enable: true,
					priority: -100,
					filters: []
				};
			}
			fetchCategories().then((c) => (categories = c));
			fetchScripts().then((s) => (scripts = s));
		}
	});

	function addFilter() {
		draft.filters.push({
			name: 'New Filter',
			enabled: true,
			title: '',
			body: '',
			cat: '',
			pp: '',
			script: '',
			priority: -100,
			type: 'require',
			size_from: 0,
			size_to: 0,
			age: 0
		});
	}

	function removeFilter(index: number) {
		draft.filters = draft.filters.filter((_, i) => i !== index);
	}

	function handleSave() {
		if (!draft.name || !draft.uri) return;
		onsave(draft);
		open = false;
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-[60] bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-[70] flex h-[90vh] w-full max-w-3xl -translate-x-1/2 -translate-y-1/2 flex-col rounded-lg border bg-white shadow-xl overflow-hidden">
			<div class="p-6 border-b">
				<Dialog.Title class="text-lg font-semibold">
					{feed ? 'Edit RSS Feed' : 'Add RSS Feed'}
				</Dialog.Title>
			</div>

			<div class="flex-1 overflow-y-auto p-6 space-y-6">
				<!-- Basic Info -->
				<div class="grid grid-cols-2 gap-4">
					<div class="col-span-2 space-y-1.5">
						<label for="rss-name" class="text-sm font-medium">Feed Name</label>
						<Input id="rss-name" bind:value={draft.name} placeholder="e.g. My Indexer" disabled={!!feed} />
					</div>
					<div class="col-span-2 space-y-1.5">
						<label for="rss-uri" class="text-sm font-medium">Feed URL (URI)</label>
						<Input id="rss-uri" bind:value={draft.uri} placeholder="https://..." />
					</div>
					<div class="space-y-1.5">
						<label for="rss-cat" class="text-sm font-medium">Default Category</label>
						<select id="rss-cat" bind:value={draft.cat} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
							<option value="">Default</option>
							{#each categories as cat}
								<option value={cat}>{cat}</option>
							{/each}
						</select>
					</div>
					<div class="space-y-1.5">
						<label for="rss-prio" class="text-sm font-medium">Default Priority</label>
						<select id="rss-prio" bind:value={draft.priority} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
							<option value={-100}>Default</option>
							<option value={2}>Force</option>
							<option value={1}>High</option>
							<option value={0}>Normal</option>
							<option value={-1}>Low</option>
						</select>
					</div>
				</div>

				<Separator />

				<!-- Filters Section -->
				<div class="space-y-4">
					<div class="flex items-center justify-between">
						<h4 class="text-sm font-bold uppercase tracking-wider text-gray-500">Filters</h4>
						<Button size="sm" variant="outline" onclick={addFilter}>+ Add Filter</Button>
					</div>

					{#if draft.filters.length === 0}
						<div class="rounded-md border border-dashed p-8 text-center text-sm text-gray-400 italic">
							No filters defined. Items will be accepted based on feed defaults.
						</div>
					{:else}
						<div class="space-y-4">
							{#each draft.filters as filter, i}
								<div class="rounded-lg border bg-gray-50/50 p-4 space-y-3 relative">
									<button 
										class="absolute top-2 right-2 text-gray-400 hover:text-red-500"
										onclick={() => removeFilter(i)}
										title="Remove filter"
									>
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4">
											<path d="M5.28 4.22a.75.75 0 0 0-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 1 0 1.06 1.06L8 9.06l2.72 2.72a.75.75 0 1 0 1.06-1.06L9.06 8l2.72-2.72a.75.75 0 0 0-1.06-1.06L8 6.94 5.28 4.22Z" />
										</svg>
									</button>

									<div class="grid grid-cols-3 gap-3">
										<div class="col-span-2 space-y-1">
											<label for="filter-name-{i}" class="text-[10px] font-bold uppercase text-gray-400">Filter Name / Label</label>
											<Input id="filter-name-{i}" bind:value={filter.name} class="h-8 text-xs" />
										</div>
										<div class="space-y-1">
											<label for="filter-type-{i}" class="text-[10px] font-bold uppercase text-gray-400">Match Type</label>
											<select id="filter-type-{i}" bind:value={filter.type} class="w-full h-8 rounded-md border border-input bg-white px-2 py-0 text-xs">
												<option value="require">Require</option>
												<option value="must_not_match">Exclude</option>
												<option value="ignore">Ignore</option>
											</select>
										</div>
										<div class="col-span-3 space-y-1">
											<label for="filter-title-{i}" class="text-[10px] font-bold uppercase text-gray-400">Regex (Title)</label>
											<Input id="filter-title-{i}" bind:value={filter.title} placeholder="Regex to match against title" class="h-8 text-xs" />
										</div>
										<div class="space-y-1">
											<label for="filter-cat-{i}" class="text-[10px] font-bold uppercase text-gray-400">Category</label>
											<select id="filter-cat-{i}" bind:value={filter.cat} class="w-full h-8 rounded-md border border-input bg-white px-2 py-0 text-xs">
												<option value="">(Inherit)</option>
												{#each categories as cat}
													<option value={cat}>{cat}</option>
												{/each}
											</select>
										</div>
										<div class="space-y-1">
											<label for="filter-priority-{i}" class="text-[10px] font-bold uppercase text-gray-400">Priority</label>
											<select id="filter-priority-{i}" bind:value={filter.priority} class="w-full h-8 rounded-md border border-input bg-white px-2 py-0 text-xs">
												<option value={-100}>(Inherit)</option>
												<option value={2}>Force</option>
												<option value={1}>High</option>
												<option value={0}>Normal</option>
												<option value={-1}>Low</option>
											</select>
										</div>
										<div class="flex items-end pb-1.5">
											<label class="flex items-center gap-2 text-xs font-medium cursor-pointer">
												<input type="checkbox" bind:checked={filter.enabled} class="rounded border-gray-300" />
												Active
											</label>
										</div>
									</div>
								</div>
							{/each}
						</div>
					{/if}
				</div>
			</div>

			<div class="p-6 border-t bg-gray-50 flex justify-end gap-3">
				<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
				<Button onclick={handleSave} disabled={!draft.name || !draft.uri}>Save Feed</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
