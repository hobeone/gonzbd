<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import type { CategoryConfig } from '$lib/types';

	let {
		configData,
		onAddCategory,
		onEditCategory,
		onDeleteCategory
	}: {
		configData: Record<string, any>;
		onAddCategory: () => void;
		onEditCategory: (cat: CategoryConfig) => void;
		onDeleteCategory: (name: string) => void;
	} = $props();
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">Categories</h3>
			<p class="text-sm text-muted-foreground">Define how different types of downloads are handled.</p>
		</div>
		<Button size="sm" onclick={onAddCategory}>+ Add Category</Button>
	</div>
	<Separator />

	<div class="space-y-4">
		<div class="overflow-hidden rounded-md border">
			<table class="w-full text-left text-sm">
				<thead class="bg-gray-50 text-xs uppercase text-gray-500">
					<tr>
						<th class="px-4 py-2">Name</th>
						<th class="px-4 py-2">Path</th>
						<th class="px-4 py-2">PP</th>
						<th class="px-4 py-2 text-right">Actions</th>
					</tr>
				</thead>
				<tbody class="divide-y">
					{#each configData.categories as cat}
						<tr>
							<td class="px-4 py-3 font-medium">{cat.name}</td>
							<td class="px-4 py-3 text-gray-600">{cat.dir || '(default)'}</td>
							<td class="px-4 py-3">
								<div class="flex gap-1">
									{#if cat.pp & 1}<Badge variant="outline" class="px-1 py-0 text-[10px]">R</Badge>{/if}
									{#if cat.pp & 2}<Badge variant="outline" class="px-1 py-0 text-[10px]">U</Badge>{/if}
									{#if cat.pp & 4}<Badge variant="outline" class="px-1 py-0 text-[10px]">D</Badge>{/if}
								</div>
							</td>
							<td class="px-4 py-3 text-right">
								<div class="flex justify-end gap-1">
									<Button variant="ghost" size="xs" onclick={() => onEditCategory(cat)}>Edit</Button>
									<Button variant="ghost" size="xs" class="text-red-600" disabled={cat.name === '*' || cat.name === 'Default'} onclick={() => onDeleteCategory(cat.name)}>Delete</Button>
								</div>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	</div>
</section>
