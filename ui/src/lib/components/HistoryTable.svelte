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
		<svg
			xmlns="http://www.w3.org/2000/svg"
			viewBox="0 0 16 16"
			fill="currentColor"
			class="absolute left-2.5 top-2.5 size-4 text-gray-400"
		>
			<path
				fill-rule="evenodd"
				d="M9.965 11.026a5 5 0 1 1 1.06-1.06l2.755 2.754a.75.75 0 1 1-1.06 1.06l-2.755-2.754ZM10.5 7a3.5 3.5 0 1 1-7 0 3.5 3.5 0 0 1 7 0Z"
				clip-rule="evenodd"
			/>
		</svg>
		<Input
			type="search"
			placeholder="Search history..."
			class="pl-8"
			bind:value={localSearch}
		/>
	</div>

	<div class="flex items-center gap-4">
		<label class="flex cursor-pointer items-center gap-2 text-sm text-gray-600 dark:text-gray-400">
			<input
				type="checkbox"
				checked={getHistoryFailedOnly()}
				onchange={(e) => setHistoryFailedOnly(e.currentTarget.checked)}
				class="size-4 rounded border-gray-300 text-blue-600 focus:ring-blue-500"
			/>
			<span>Failed only</span>
		</label>
	</div>
</div>

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
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-sm" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-lg outline-none"
		>
			<div class="mb-4">
				<Dialog.Title class="text-lg font-bold">Delete History Item</Dialog.Title>
				<Dialog.Description class="mt-2 text-sm text-gray-500">
					Are you sure you want to delete <span class="font-semibold text-gray-900"
						>{deleteTarget?.name}</span
					> from history?
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

				<div class="mt-6 flex justify-end gap-3">							<Button variant="outline" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
							<Button variant="destructive" onclick={remove} disabled={acting}>
								{acting ? 'Deleting...' : 'Delete Item'}
							</Button>
						</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
