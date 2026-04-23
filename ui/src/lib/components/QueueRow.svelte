<script lang="ts">
	import type { QueueSlot } from '$lib/types';
	import { Progress } from '$lib/components/ui/progress';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import * as Dialog from '$lib/components/ui/dialog';
	import { pauseJob, resumeJob, deleteJob } from '$lib/stores/queue.svelte';

	let { slot }: { slot: QueueSlot } = $props();

	let acting = $state(false);
	let showDeleteConfirm = $state(false);
	let deleteFiles = $state(false);

	function pct(): number {
		return parseFloat(slot.percentage) || 0;
	}

	function isPaused(): boolean {
		return slot.status === 'Paused';
	}

	async function togglePause() {
		acting = true;
		try {
			if (isPaused()) {
				await resumeJob(slot.nzo_id);
			} else {
				await pauseJob(slot.nzo_id);
			}
		} finally {
			acting = false;
		}
	}

	async function remove() {
		acting = true;
		try {
			await deleteJob(slot.nzo_id, deleteFiles);
			showDeleteConfirm = false;
		} finally {
			acting = false;
		}
	}
</script>

<tr class="border-b hover:bg-gray-50">
	<td class="px-4 py-3">
		<div class="flex items-center gap-2">
			<div class="font-medium text-gray-900">{slot.name || slot.filename}</div>
			{#if slot.warning}
				<div class="flex items-center text-amber-600" title={slot.warning}>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4">
						<path
							fill-rule="evenodd"
							d="M6.701 2.25c.577-1 1.419-1 1.998 0l5.156 8.93c.577 1 .158 1.82-1 1.82H3.145c-1.158 0-1.577-.82-1-1.82l5.156-8.93ZM8 5.5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 5.5Zm0 6a.625.625 0 1 0 0-1.25.625.625 0 0 0 0 1.25Z"
							clip-rule="evenodd"
						/>
					</svg>
					<span class="ml-1 text-xs font-semibold">{slot.warning}</span>
				</div>
			{/if}
		</div>
		<div class="mt-1 flex items-center gap-2">
			<Progress value={pct()} max={100} class="h-2 flex-1" />
			<span class="text-xs font-mono text-gray-500">{slot.percentage}%</span>
		</div>
	</td>
	<td class="px-4 py-3 text-sm text-gray-600">{slot.size}</td>
	<td class="px-4 py-3 text-sm text-gray-600">{slot.sizeleft}</td>
	<td class="px-4 py-3">
		<Badge variant={isPaused() ? 'outline' : 'default'} class="text-xs">
			{slot.status}
		</Badge>
	</td>
	<td class="px-4 py-3 text-sm text-gray-600">{slot.category || '*'}</td>
	<td class="px-4 py-3">
		<div class="flex gap-1">
			<Button variant="ghost" size="icon-xs" onclick={togglePause} disabled={acting} title={isPaused() ? 'Resume' : 'Pause'}>
				{#if isPaused()}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path d="M4.5 2A1.5 1.5 0 0 0 3 3.5v9a1.5 1.5 0 0 0 2.3 1.27l7-4.5a1.5 1.5 0 0 0 0-2.54l-7-4.5A1.5 1.5 0 0 0 4.5 2Z" />
					</svg>
				{:else}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path
							d="M4.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75A.75.75 0 0 0 5.5 2h-1ZM10.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75a.75.75 0 0 0-.75-.75h-1Z"
						/>
					</svg>
				{/if}
			</Button>

			<Dialog.Root bind:open={showDeleteConfirm}>
				<Button variant="ghost" size="icon-xs" onclick={() => (showDeleteConfirm = true)} disabled={acting} title="Delete">
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 text-red-500">
						<path
							fill-rule="evenodd"
							d="M5 3.25V4H2.75a.75.75 0 0 0 0 1.5h.3l.815 8.15A1.5 1.5 0 0 0 5.357 15h5.285a1.5 1.5 0 0 0 1.493-1.35l.815-8.15h.3a.75.75 0 0 0 0-1.5H11v-.75A2.25 2.25 0 0 0 8.75 1h-1.5A2.25 2.25 0 0 0 5 3.25Zm2.25-.75a.75.75 0 0 0-.75.75V4h3v-.75a.75.75 0 0 0-.75-.75h-1.5ZM6.05 6a.75.75 0 0 1 .787.713l.275 5.5a.75.75 0 0 1-1.498.075l-.275-5.5A.75.75 0 0 1 6.05 6Zm3.9 0a.75.75 0 0 1 .712.787l-.275 5.5a.75.75 0 0 1-1.498-.075l.275-5.5a.75.75 0 0 1 .786-.711Z"
							clip-rule="evenodd"
						/>
					</svg>
				</Button>

				<Dialog.Content class="max-w-md">
					<Dialog.Header>
						<Dialog.Title>Delete Job</Dialog.Title>
						<Dialog.Description>
							Are you sure you want to delete <span class="font-semibold text-gray-900">{slot.name || slot.filename}</span>?
						</Dialog.Description>
					</Dialog.Header>

					<div class="py-4">
						<label class="flex cursor-pointer items-center gap-2 text-sm">
							<input type="checkbox" bind:checked={deleteFiles} class="size-4 rounded border-gray-300 text-red-600 focus:ring-red-500" />
							<span>Also delete downloaded files from disk</span>
						</label>
					</div>

					<Dialog.Footer>
						<Button variant="outline" onclick={() => (showDeleteConfirm = false)}>Cancel</Button>
						<Button variant="destructive" onclick={remove} disabled={acting}>
							{acting ? 'Deleting...' : 'Delete Job'}
						</Button>
					</Dialog.Footer>
				</Dialog.Content>
			</Dialog.Root>
		</div>
	</td>
</tr>
