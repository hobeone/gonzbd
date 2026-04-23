<script lang="ts">
	import { getQueueSlots, getQueue, getError, deleteJob } from '$lib/stores/queue.svelte';
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import QueueRow from './QueueRow.svelte';
	import type { QueueSlot } from '$lib/types';

	function slots() {
		return getQueueSlots();
	}

	function totalSlots(): number {
		return getQueue()?.noofslots_total ?? 0;
	}

	let deleteTarget = $state<QueueSlot | null>(null);
	let showDeleteConfirm = $state(false);
	let deleteFiles = $state(false);
	let acting = $state(false);

	function openDelete(slot: QueueSlot) {
		deleteTarget = slot;
		deleteFiles = false;
		showDeleteConfirm = true;
	}

	async function remove() {
		if (!deleteTarget) return;
		acting = true;
		try {
			await deleteJob(deleteTarget.nzo_id, deleteFiles);
			showDeleteConfirm = false;
		} finally {
			acting = false;
		}
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
					<th class="px-4 py-3">Progress</th>
					<th class="px-4 py-3">Size</th>
					<th class="px-4 py-3">Left</th>
					<th class="px-4 py-3">Status</th>
					<th class="px-4 py-3">Category</th>
					<th class="px-4 py-3">Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each slots() as slot (slot.nzo_id)}
					<QueueRow {slot} onremove={() => openDelete(slot)} />
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

<Dialog.Root bind:open={showDeleteConfirm}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-sm" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-lg outline-none"
		>
			<div class="mb-4">
				<Dialog.Title class="text-lg font-bold">Delete Job</Dialog.Title>
				<Dialog.Description class="mt-2 text-sm text-gray-500">
					Are you sure you want to delete <span class="font-semibold text-gray-900"
						>{deleteTarget?.name || deleteTarget?.filename}</span
					>?
				</Dialog.Description>
			</div>

			<div class="py-4 text-gray-900">
				<label class="flex cursor-pointer items-center gap-2 text-sm">
					<input
						type="checkbox"
						bind:checked={deleteFiles}
						class="size-4 rounded border-gray-300 text-red-600 focus:ring-red-500"
					/>
					<span>Also delete downloaded files from disk</span>
				</label>
			</div>

			<div class="mt-6 flex justify-end gap-3">
				<Button variant="outline" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
				<Button variant="destructive" onclick={remove} disabled={acting}>
					{acting ? 'Deleting...' : 'Delete Job'}
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
