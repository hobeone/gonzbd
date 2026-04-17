<script lang="ts">
	import { getQueueSlots, getQueue, getError } from '$lib/stores/queue.svelte';
	import QueueRow from './QueueRow.svelte';

	function slots() {
		return getQueueSlots();
	}

	function totalSlots(): number {
		return getQueue()?.noofslots_total ?? 0;
	}
</script>

{#if getError()}
	<div class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
		API error: {getError()}
	</div>
{:else if slots().length === 0}
	<div class="rounded-lg border bg-white p-8 text-center text-gray-500">
		{#if getQueue() === null}
			Loading...
		{:else}
			Queue is empty
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-lg border bg-white">
		<table class="w-full text-left">
			<thead class="border-b bg-gray-50 text-xs uppercase text-gray-500">
				<tr>
					<th class="px-4 py-3">Name</th>
					<th class="px-4 py-3">Size</th>
					<th class="px-4 py-3">Left</th>
					<th class="px-4 py-3">Status</th>
					<th class="px-4 py-3">Category</th>
					<th class="px-4 py-3">Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each slots() as slot (slot.nzo_id)}
					<QueueRow {slot} />
				{/each}
			</tbody>
		</table>
	</div>
	{#if totalSlots() > slots().length}
		<p class="mt-2 text-center text-xs text-gray-500">
			Showing {slots().length} of {totalSlots()} items
		</p>
	{/if}
{/if}
