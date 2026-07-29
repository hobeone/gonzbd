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
	import Modal from '$lib/components/ui/Modal.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import Search from '@lucide/svelte/icons/search';
	import Loader2 from '@lucide/svelte/icons/loader-2';
	import QueueRow from './QueueRow.svelte';
	import Pagination from './Pagination.svelte';
	import AddNzbDialog from './AddNzbDialog.svelte';
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
	let addDialogOpen = $state(false);

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

<div class="mb-4 flex items-center justify-between gap-4">
	<div class="relative w-full max-w-sm">
		<Search class="absolute left-3 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
		<Input
			type="search"
			placeholder="Search queue..."
			class="pl-9 h-9 text-xs rounded-full border-border bg-card focus-visible:ring-primary"
			bind:value={localSearch}
		/>
	</div>
</div>

{#if getError()}
	<div class="rounded-2xl border border-destructive/30 bg-destructive/10 p-4 text-xs font-semibold text-destructive">
		API error: {getError()}
	</div>
{:else if slots().length === 0}
	<div class="flex h-12 items-center justify-center rounded-2xl border border-border/60 bg-card/60 px-4 text-xs font-medium text-muted-foreground">
		{#if getQueue() === null}
			<div class="flex items-center gap-2">
				<Loader2 class="size-4 animate-spin text-muted-foreground" />
				<span>Loading download queue...</span>
			</div>
		{:else}
			<span>Queue is empty</span>
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-2xl border border-border/60 bg-card shadow-sm">
		<table class="w-full text-left">
			<thead class="border-b border-border/40 bg-muted/30 text-[11px] font-bold uppercase tracking-wider text-muted-foreground">
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
			<tbody class="divide-y divide-border/30">
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

<AddNzbDialog bind:open={addDialogOpen} />

<Modal bind:open={showDeleteConfirm} class="w-full max-w-sm bg-card text-foreground p-6 border border-border">
	<h2 class="text-base font-semibold text-foreground">Delete Job</h2>
	<p class="mt-2 text-xs text-muted-foreground">
		Are you sure you want to delete <span class="font-medium text-foreground">{deleteTarget?.name || deleteTarget?.filename}</span>?
	</p>
	<div class="mt-4 flex items-center gap-2">
		<input
			type="checkbox"
			id="delete-files-queue"
			bind:checked={deleteFiles}
			class="size-4 rounded border-border text-primary focus:ring-primary"
		/>
		<label for="delete-files-queue" class="text-xs font-medium text-foreground cursor-pointer">
			Also delete downloaded files from disk
		</label>
	</div>
	<div class="mt-5 flex justify-end gap-2">
		<Button
			variant="outline"
			size="sm"
			class="rounded-xl border-border bg-card text-xs font-medium text-foreground hover:bg-muted"
			onclick={() => (showDeleteConfirm = false)}
		>
			Cancel
		</Button>
		<Button
			variant="destructive"
			size="sm"
			class="rounded-xl text-xs font-medium"
			onclick={remove}
			disabled={acting}
		>
			{acting ? 'Deleting...' : 'Delete Job'}
		</Button>
	</div>
</Modal>
