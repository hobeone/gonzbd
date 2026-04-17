<script lang="ts">
	import { getHistorySlots, getHistory, getHistoryError } from '$lib/stores/history.svelte';
	import HistoryRow from './HistoryRow.svelte';

	function slots() {
		return getHistorySlots();
	}
</script>

{#if getHistoryError()}
	<div class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
		API error: {getHistoryError()}
	</div>
{:else if slots().length === 0}
	<div class="rounded-lg border bg-white p-8 text-center text-gray-500">
		{#if getHistory() === null}
			Loading...
		{:else}
			History is empty
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-lg border bg-white">
		<table class="w-full text-left">
			<thead class="border-b bg-gray-50 text-xs uppercase text-gray-500">
				<tr>
					<th class="px-4 py-3">Name</th>
					<th class="px-4 py-3">Size</th>
					<th class="px-4 py-3">Status</th>
					<th class="px-4 py-3">Category</th>
					<th class="px-4 py-3">Completed</th>
					<th class="px-4 py-3">Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each slots() as slot (slot.nzo_id)}
					<HistoryRow {slot} />
				{/each}
			</tbody>
		</table>
	</div>
	{#if (getHistory()?.noofslots ?? 0) > slots().length}
		<p class="mt-2 text-center text-xs text-gray-500">
			Showing {slots().length} of {getHistory()?.noofslots} items
		</p>
	{/if}
{/if}
