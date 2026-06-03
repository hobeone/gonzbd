<script lang="ts">
	import { untrack } from 'svelte';
	import {
		getQueueSlots,
		getQueue,
		getError,
		deleteJob,
		getQueuePage,
		getQueueLimit,
		setQueuePage,
		getQueueSearch,
		setQueueSearch
	} from '$lib/stores/queue.svelte';
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import QueueRow from './QueueRow.svelte';
	import Pagination from './Pagination.svelte';
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

	let localSearch = $state(getQueueSearch());

	$effect(() => {
		const current = localSearch;
		const timeout = setTimeout(() => {
			untrack(() => {
				if (current !== getQueueSearch()) {
					setQueueSearch(current);
				}
			});
		}, 300);
		return () => clearTimeout(timeout);
	});
</script>

<div class="mb-4">
	<div class="relative w-full max-w-sm">
		<span class="material-symbols-outlined absolute left-3 top-1/2 -translate-y-1/2 text-lg text-m3-on-surface-variant">search</span>
		<Input
			type="search"
			placeholder="Search queue..."
			class="pl-9 h-10 rounded-full border-m3-outline focus-visible:ring-m3-primary"
			bind:value={localSearch}
		/>
	</div>
</div>

{#if getError()}
	<div class="rounded-2xl border border-destructive/20 bg-destructive/10 p-4 text-sm text-destructive font-semibold">
		API error: {getError()}
	</div>
{:else if slots().length === 0}
	<div class="rounded-3xl border border-m3-outline/20 bg-m3-surface p-8 text-center text-m3-on-surface-variant/80 font-medium">
		{#if getQueue() === null}
			Loading...
		{:else}
			Queue is empty
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-3xl border border-m3-outline/20 bg-m3-surface shadow-m3-1">
		<table class="w-full text-left">
			<thead class="border-b border-m3-outline/10 bg-m3-surface-variant/20 text-[11px] font-bold uppercase tracking-wider text-m3-on-surface-variant">
				<tr>
					<th class="px-5 py-3.5">Name</th>
					<th class="px-5 py-3.5">Progress</th>
					<th class="px-5 py-3.5">Size</th>
					<th class="px-5 py-3.5">Left</th>
					<th class="px-5 py-3.5">Status</th>
					<th class="px-5 py-3.5">Category</th>
					<th class="px-5 py-3.5 text-right">Actions</th>
				</tr>
			</thead>
			<tbody>
				{#each slots() as slot (slot.nzo_id)}
					<QueueRow {slot} onremove={() => openDelete(slot)} />
				{/each}
			</tbody>
		</table>
	</div>

	<Pagination
		total={totalSlots()}
		limit={getQueueLimit()}
		page={getQueuePage()}
		onPageChange={setQueuePage}
	/>
{/if}

<Dialog.Root bind:open={showDeleteConfirm}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-m3-outline/20 bg-m3-surface text-m3-on-surface p-6 shadow-m3-3 outline-none animate-in fade-in zoom-in-95"
		>
			<div class="mb-4">
				<Dialog.Title class="text-lg font-semibold tracking-tight text-m3-on-surface">Delete Job</Dialog.Title>
				<Dialog.Description class="mt-2 text-sm text-m3-on-surface-variant/80">
					Are you sure you want to delete <span class="inline-block max-w-[200px] sm:max-w-xs align-bottom font-bold text-m3-on-surface truncate" title={deleteTarget?.name || deleteTarget?.filename}
						>{deleteTarget?.name || deleteTarget?.filename}</span
					>?
				</Dialog.Description>
			</div>

			<div class="py-4">
				<label class="flex cursor-pointer items-center gap-2.5 text-sm text-m3-on-surface font-medium select-none">
					<input
						type="checkbox"
						bind:checked={deleteFiles}
						class="size-4 rounded-md border-m3-outline text-m3-primary focus:ring-m3-primary accent-m3-primary cursor-pointer"
					/>
					<span>Also delete downloaded files from disk</span>
				</label>
			</div>

			<div class="mt-6 flex justify-end gap-3">
				<Button variant="outline" class="rounded-full px-5 border-m3-outline text-m3-on-surface hover:bg-m3-surface-variant/50" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
				<Button variant="destructive" class="rounded-full px-5 bg-destructive text-destructive-foreground hover:bg-destructive/90" onclick={remove} disabled={acting}>
					{acting ? 'Deleting...' : 'Delete Job'}
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
