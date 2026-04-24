<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import type { RSSFeedConfig } from '$lib/types';

	let {
		configData,
		onAddFeed,
		onEditFeed,
		onDeleteFeed
	}: {
		configData: Record<string, any>;
		onAddFeed: () => void;
		onEditFeed: (feed: RSSFeedConfig) => void;
		onDeleteFeed: (name: string) => void;
	} = $props();
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">RSS Feeds</h3>
			<p class="text-sm text-muted-foreground">Automated downloads from indexers.</p>
		</div>
		<Button size="sm" onclick={onAddFeed}>+ Add Feed</Button>
	</div>
	<Separator />

	<div class="space-y-4">
		{#if configData.rss.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
				No feeds configured.
			</div>
		{:else}
			<div class="overflow-hidden rounded-md border">
				<table class="w-full text-left text-sm">
					<thead class="bg-gray-50 text-xs uppercase text-gray-500">
						<tr>
							<th class="px-4 py-2">Name</th>
							<th class="px-4 py-2">URI</th>
							<th class="px-4 py-2 text-right">Actions</th>
						</tr>
					</thead>
					<tbody class="divide-y">
						{#each configData.rss as feed}
							<tr class={feed.enable ? '' : 'opacity-50'}>
								<td class="px-4 py-3 font-medium">{feed.name}</td>
								<td class="px-4 py-3 text-gray-600 truncate max-w-xs">{feed.uri}</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-1">
										<Button variant="ghost" size="xs" onclick={() => onEditFeed(feed)}>Edit</Button>
										<Button variant="ghost" size="xs" class="text-red-600" onclick={() => onDeleteFeed(feed.name)}>Delete</Button>
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
