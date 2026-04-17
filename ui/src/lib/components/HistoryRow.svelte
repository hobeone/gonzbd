<script lang="ts">
	import type { HistorySlot } from '$lib/types';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { deleteHistoryItem } from '$lib/stores/history.svelte';

	let { slot }: { slot: HistorySlot } = $props();

	let acting = $state(false);

	function statusVariant(): 'default' | 'destructive' | 'outline' {
		if (slot.status === 'Completed') return 'default';
		if (slot.status === 'Failed') return 'destructive';
		return 'outline';
	}

	function completedDate(): string {
		if (!slot.completed) return '--';
		return new Date(slot.completed * 1000).toLocaleString();
	}

	async function remove() {
		acting = true;
		try {
			await deleteHistoryItem(slot.nzo_id);
		} finally {
			acting = false;
		}
	}
</script>

<tr class="border-b hover:bg-gray-50">
	<td class="px-4 py-3">
		<div class="font-medium text-gray-900">{slot.name}</div>
		{#if slot.fail_message}
			<div class="mt-0.5 text-xs text-red-600">{slot.fail_message}</div>
		{/if}
	</td>
	<td class="px-4 py-3 text-sm text-gray-600">{slot.size}</td>
	<td class="px-4 py-3">
		<Badge variant={statusVariant()} class="text-xs">
			{slot.status}
		</Badge>
	</td>
	<td class="px-4 py-3 text-sm text-gray-600">{slot.category || '*'}</td>
	<td class="px-4 py-3 text-sm text-gray-600">{completedDate()}</td>
	<td class="px-4 py-3">
		<Button variant="ghost" size="icon-xs" onclick={remove} disabled={acting} title="Delete">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 text-red-500">
				<path fill-rule="evenodd" d="M5 3.25V4H2.75a.75.75 0 0 0 0 1.5h.3l.815 8.15A1.5 1.5 0 0 0 5.357 15h5.285a1.5 1.5 0 0 0 1.493-1.35l.815-8.15h.3a.75.75 0 0 0 0-1.5H11v-.75A2.25 2.25 0 0 0 8.75 1h-1.5A2.25 2.25 0 0 0 5 3.25Zm2.25-.75a.75.75 0 0 0-.75.75V4h3v-.75a.75.75 0 0 0-.75-.75h-1.5ZM6.05 6a.75.75 0 0 1 .787.713l.275 5.5a.75.75 0 0 1-1.498.075l-.275-5.5A.75.75 0 0 1 6.05 6Zm3.9 0a.75.75 0 0 1 .712.787l-.275 5.5a.75.75 0 0 1-1.498-.075l.275-5.5a.75.75 0 0 1 .786-.711Z" clip-rule="evenodd" />
			</svg>
		</Button>
	</td>
</tr>
