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

<Dialog.Root bind:open={showDeleteConfirm}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/60 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-2xl border border-border bg-card text-card-foreground p-6 shadow-2xl outline-none animate-in fade-in zoom-in-95"
		>
			<div class="mb-4">
				<Dialog.Title class="text-base font-bold tracking-tight text-foreground">Delete Job</Dialog.Title>
				<Dialog.Description class="mt-2 text-xs text-muted-foreground leading-relaxed">
					Are you sure you want to delete <span class="inline-block max-w-[200px] sm:max-w-xs align-bottom font-bold text-foreground truncate" title={deleteTarget?.name || deleteTarget?.filename}
						>{deleteTarget?.name || deleteTarget?.filename}</span
					>?
				</Dialog.Description>
			</div>

			<div class="py-3">
				<label class="flex cursor-pointer items-center gap-2.5 text-xs text-foreground font-medium select-none">
					<input
						type="checkbox"
						bind:checked={deleteFiles}
						class="size-4 rounded border-border text-primary focus:ring-primary accent-primary cursor-pointer"
					/>
					<span>Also delete downloaded files from disk</span>
				</label>
			</div>

			<div class="mt-6 flex justify-end gap-2.5">
				<Button variant="outline" size="sm" class="rounded-full px-4 border-border text-foreground hover:bg-muted" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
				<Button variant="destructive" size="sm" class="rounded-full px-4 bg-destructive text-destructive-foreground hover:bg-destructive/90" onclick={remove} disabled={acting}>
					{acting ? 'Deleting...' : 'Delete Job'}
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
