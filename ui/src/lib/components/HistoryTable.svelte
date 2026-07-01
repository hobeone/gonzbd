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
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
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
		<span class="material-symbols-outlined absolute left-3 top-1/2 -translate-y-1/2 text-lg text-m3-on-surface-variant">search</span>
		<Input
			type="search"
			placeholder="Search history..."
			class="pl-9 h-10 rounded-full border-m3-outline focus-visible:ring-m3-primary"
			bind:value={localSearch}
		/>
	</div>

	<div class="flex items-center gap-4">
		<label class="flex cursor-pointer items-center gap-2.5 text-sm text-m3-on-surface font-medium select-none">
			<input
				type="checkbox"
				checked={getHistoryFailedOnly()}
				onchange={(e) => setHistoryFailedOnly(e.currentTarget.checked)}
				class="size-4 rounded-md border-m3-outline text-m3-primary focus:ring-m3-primary cursor-pointer accent-m3-primary"
			/>
			<span>Failed only</span>
		</label>
	</div>
</div>

{#if getHistoryError()}
	<div class="rounded-2xl border border-destructive/20 bg-destructive/10 p-4 text-sm text-destructive font-semibold">
		API error: {getHistoryError()}
	</div>
{:else if slots().length === 0}
	<div class="rounded-3xl border border-m3-outline/20 bg-m3-surface p-8 text-center text-m3-on-surface-variant/80 font-medium">
		{#if getHistory() === null}
			Loading...
		{:else}
			History is empty
		{/if}
	</div>
{:else}
	<div class="overflow-x-auto rounded-3xl border border-m3-outline/20 bg-m3-surface shadow-m3-1">
		<table class="w-full text-left">
			<thead class="border-b border-m3-outline/10 bg-m3-surface-variant/20 text-[11px] font-bold uppercase tracking-wider text-m3-on-surface-variant">
				<tr>
					<th class="px-5 py-3.5">Name</th>
					<th class="px-5 py-3.5">Size</th>
					<th class="px-5 py-3.5">Status</th>
					<th class="px-5 py-3.5">Category</th>
					<th class="px-5 py-3.5">Completed</th>
					<th class="px-5 py-3.5 text-right">Actions</th>
				</tr>
			</thead>
			<tbody>
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

<Dialog.Root bind:open={showDeleteConfirm}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-m3-outline/20 bg-m3-surface text-m3-on-surface p-6 shadow-m3-3 outline-none animate-in fade-in zoom-in-95"
		>
			<div class="mb-4">
				<Dialog.Title class="text-lg font-semibold tracking-tight text-m3-on-surface">Delete History Item</Dialog.Title>
				<Dialog.Description class="mt-2 text-sm text-m3-on-surface-variant/80">
					Are you sure you want to delete <span class="inline-block max-w-[200px] sm:max-w-xs align-bottom font-bold text-m3-on-surface truncate" title={deleteTarget?.name}
						>{deleteTarget?.name}</span
					> from history?
				</Dialog.Description>
			</div>

			<div class="py-4">
				<label class="flex cursor-pointer items-center gap-2.5 text-sm text-m3-on-surface font-medium select-none">
					<input
						type="checkbox"
						bind:checked={deleteFiles}
						class="size-4 rounded-md border-m3-outline text-m3-primary focus:ring-m3-primary cursor-pointer accent-m3-primary"
					/>
					<span>Also delete downloaded files from disk</span>
				</label>
			</div>

			<div class="mt-6 flex justify-end gap-3">
				<Button variant="outline" class="rounded-full px-5 border-m3-outline text-m3-on-surface hover:bg-m3-surface-variant/50" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
				<Button variant="destructive" class="rounded-full px-5 bg-destructive text-destructive-foreground hover:bg-red-600 dark:hover:bg-red-500 hover:text-white dark:hover:text-white" onclick={remove} disabled={acting}>
					{acting ? 'Deleting...' : 'Delete Item'}
				</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
