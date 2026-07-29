<script lang="ts">
	import { untrack } from 'svelte';
	import {
		getHistorySlots,
		getHistory,
		getHistoryError,
		deleteHistoryItem,
		getHistoryPage,
		getHistoryLimit,
		setHistoryPage,
		getHistoryFailedOnly,
		setHistoryFailedOnly,
		getHistorySearch,
		setHistorySearch
	} from '$lib/stores/history.svelte';
	import Modal from '$lib/components/ui/Modal.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import Search from '@lucide/svelte/icons/search';
	import Loader2 from '@lucide/svelte/icons/loader-2';
	import HistoryRow from './HistoryRow.svelte';
	import Pagination from './Pagination.svelte';
	import type { HistorySlot } from '$lib/types';
	import { showToast } from '$lib/stores/warnings.svelte';

	function slots() {
		return getHistorySlots();
	}

	let deleteTarget = $state<HistorySlot | null>(null);
	let showDeleteConfirm = $state(false);
	let deleteFiles = $state(false);
	let acting = $state(false);

	$effect(() => {
		if (showDeleteConfirm) {
			deleteFiles = false;
		}
	});

	function openDelete(slot: HistorySlot) {
		deleteTarget = slot;
		showDeleteConfirm = true;
	}

	async function remove() {
		if (!deleteTarget) return;
		acting = true;
		try {
			await deleteHistoryItem(deleteTarget.nzo_id, deleteFiles);
			showDeleteConfirm = false;
		} catch (e) {
			showToast(e instanceof Error ? e.message : String(e));
		} finally {
			acting = false;
		}
	}

	let localSearch = $state(getHistorySearch());

	$effect(() => {
		// Only trigger when localSearch changes
		const current = localSearch;
		
		const timeout = setTimeout(() => {
			untrack(() => {
				if (current !== getHistorySearch()) {
					setHistorySearch(current);
				}
			});
		}, 300);
		return () => clearTimeout(timeout);
	});
</script>

<div class="mb-4 flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
	<div class="relative w-full max-w-sm">
		<Search class="absolute left-3 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
		<Input
			type="search"
			placeholder="Search history..."
			class="pl-9 h-9 text-xs rounded-full border-border bg-card focus-visible:ring-primary"
			bind:value={localSearch}
		/>
	</div>

	<div class="flex items-center gap-4">
		<label class="flex cursor-pointer items-center gap-2.5 text-xs text-foreground font-medium select-none">
			<input
				type="checkbox"
				checked={getHistoryFailedOnly()}
				onchange={(e) => setHistoryFailedOnly(e.currentTarget.checked)}
				class="size-4 rounded border-border text-primary focus:ring-primary cursor-pointer accent-primary"
			/>
			<span>Failed only</span>
		</label>
	</div>
</div>

{#if getHistoryError()}
	<div class="rounded-2xl border border-destructive/30 bg-destructive/10 p-4 text-xs font-semibold text-destructive">
		API error: {getHistoryError()}
	</div>
{:else if slots().length === 0}
	<div class="flex h-12 items-center justify-center rounded-2xl border border-border/60 bg-card/60 px-4 text-xs font-medium text-muted-foreground">
		{#if getHistory() === null}
			<div class="flex items-center gap-2">
				<Loader2 class="size-4 animate-spin text-muted-foreground" />
				<span>Loading download history...</span>
			</div>
		{:else}
			<span>History is empty</span>
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-2xl border border-border/60 bg-card shadow-sm">
		<table class="w-full text-left">
			<thead class="border-b border-border/40 bg-muted/30 text-[11px] font-bold uppercase tracking-wider text-muted-foreground">
				<tr>
					<th class="px-5 py-3.5">Name</th>
					<th class="px-5 py-3.5">Size</th>
					<th class="px-5 py-3.5">Status</th>
					<th class="px-5 py-3.5">Category</th>
					<th class="px-5 py-3.5">Completed</th>
					<th class="px-5 py-3.5 text-right">Actions</th>
				</tr>
			</thead>
			<tbody class="divide-y divide-border/30">
				{#each slots() as slot (slot.nzo_id)}
					<HistoryRow {slot} onremove={() => openDelete(slot)} />
				{/each}
			</tbody>
		</table>
	</div>

	<Pagination
		total={getHistory()?.noofslots ?? 0}
		limit={getHistoryLimit()}
		page={getHistoryPage()}
		onPageChange={setHistoryPage}
	/>
{/if}

<Modal bind:open={showDeleteConfirm} class="w-full max-w-sm bg-card text-foreground p-6 border border-border">
	<h2 class="text-base font-semibold text-foreground">Delete History Item</h2>
	<p class="mt-2 text-xs text-muted-foreground">
		Are you sure you want to delete <span class="font-medium text-foreground">{deleteTarget?.name}</span> from history?
	</p>
	<div class="mt-4 flex items-center gap-2">
		<input
			type="checkbox"
			id="delete-files-history"
			bind:checked={deleteFiles}
			class="size-4 rounded border-border text-primary focus:ring-primary"
		/>
		<label for="delete-files-history" class="text-xs font-medium text-foreground cursor-pointer">
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
			{acting ? 'Deleting...' : 'Delete Item'}
		</Button>
	</div>
</Modal>
