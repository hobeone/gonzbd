<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import type { SorterConfig } from '$lib/types';

	let {
		configData,
		onAddSorter,
		onEditSorter,
		onDeleteSorter
	}: {
		configData: Record<string, any>;
		onAddSorter: () => void;
		onEditSorter: (sorter: SorterConfig) => void;
		onDeleteSorter: (name: string) => void;
	} = $props();
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">Sorters</h3>
			<p class="text-sm text-muted-foreground">Automated file renaming based on media metadata.</p>
		</div>
		<Button size="sm" onclick={onAddSorter}>+ Add Sorter</Button>
	</div>
	<Separator />

	<div class="space-y-4">
		{#if configData.sorters.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
				No sorters configured.
			</div>
		{:else}
			<div class="overflow-hidden rounded-md border">
				<table class="w-full text-left text-sm">
					<thead class="bg-gray-50 text-xs uppercase text-gray-500">
						<tr>
							<th class="px-4 py-2">Name</th>
							<th class="px-4 py-2">Template</th>
							<th class="px-4 py-2 text-right">Actions</th>
						</tr>
					</thead>
					<tbody class="divide-y">
						{#each configData.sorters as sorter}
							<tr class={sorter.is_active ? '' : 'opacity-50'}>
								<td class="px-4 py-3 font-medium">{sorter.name}</td>
								<td class="px-4 py-3 font-mono text-xs text-gray-600 truncate max-w-xs">{sorter.sort_string}</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-1">
										<Button variant="ghost" size="xs" onclick={() => onEditSorter(sorter)}>Edit</Button>
										<Button variant="ghost" size="xs" class="text-red-600" onclick={() => onDeleteSorter(sorter.name)}>Delete</Button>
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</div>
</section>
