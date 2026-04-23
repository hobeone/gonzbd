<script lang="ts">
	import type { HistorySlot } from '$lib/types';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { retryHistoryJob } from '$lib/stores/history.svelte';
	import { showToast } from '$lib/stores/warnings.svelte';

	let { slot, onremove }: { slot: HistorySlot; onremove: () => void } = $props();

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

	async function retry() {
		acting = true;
		try {
			await retryHistoryJob(slot.nzo_id);
		} catch (e) {
			showToast(e instanceof Error ? e.message : String(e));
		} finally {
			acting = false;
		}
	}

	let expanded = $state(false);

	function toggle() {
		expanded = !expanded;
	}

	function formatSpeed(bytes: number, seconds: number): string {
		if (seconds <= 0) return '0 B/s';
		const bps = bytes / seconds;
		if (bps < 1024) return `${Math.round(bps)} B/s`;
		if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
		return `${(bps / (1024 * 1024)).toFixed(1)} MB/s`;
	}

	function formatDuration(seconds: number): string {
		if (seconds < 60) return `${seconds}s`;
		const mins = Math.floor(seconds / 60);
		const secs = seconds % 60;
		if (mins < 60) return `${mins}m ${secs}s`;
		const hours = Math.floor(mins / 60);
		const remainingMins = mins % 60;
		return `${hours}h ${remainingMins}m`;
	}
</script>

<tr class="border-b hover:bg-gray-50 cursor-pointer text-gray-900 dark:text-gray-100" onclick={toggle}>
	<td class="px-4 py-3">
		<div class="flex items-center gap-2">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 text-gray-400 transition-transform {expanded ? 'rotate-90' : ''}">
				<path d="M5.75 3a.75.75 0 0 0-.75.75v8.5c0 .414.336.75.75.75h.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-.5ZM10.25 3a.75.75 0 0 0-.75.75v8.5c0 .414.336.75.75.75h.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-.5Z" class="hidden" />
				<path d="M6.22 3.22a.75.75 0 0 1 1.06 0l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 0 1 0-1.06Z" />
			</svg>
			<div class="font-medium">{slot.name}</div>
		</div>
		{#if slot.fail_message}
			<div class="ml-6 mt-0.5 text-xs text-red-600">{slot.fail_message}</div>
		{/if}
	</td>
	<td class="px-4 py-3 text-sm">{slot.size}</td>
	<td class="px-4 py-3">
		<Badge variant={statusVariant()} class="text-xs">
			{slot.status}
		</Badge>
	</td>
	<td class="px-4 py-3 text-sm">{slot.category || '*'}</td>
	<td class="px-4 py-3 text-sm">{completedDate()}</td>
	<td class="px-4 py-3 flex gap-1" onclick={(e) => e.stopPropagation()}>
		{#if slot.status === 'Failed'}
			<Button variant="ghost" size="icon-xs" onclick={retry} disabled={acting} title="Retry">
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 text-blue-600">
					<path fill-rule="evenodd" d="M13.836 2.477a.75.75 0 0 1 .75.75v3.182a.75.75 0 0 1-.75.75h-3.182a.75.75 0 0 1 0-1.5h1.371A6.002 6.002 0 0 0 2.5 8c0 .88.192 1.715.534 2.464a.75.75 0 1 1-1.37.62A7.502 7.502 0 0 1 1 8a7.502 7.502 0 0 1 11.215-6.527V1.227a.75.75 0 0 1 .75-.75h.871ZM1 8c0-.88.192-1.715.534-2.464a.75.75 0 1 0-1.37-.62A7.502 7.502 0 0 0 1 8a7.502 7.502 0 0 0 11.215 6.527v.246a.75.75 0 0 0 1.5 0v-3.182a.75.75 0 0 0-.75-.75h-3.182a.75.75 0 0 0 0 1.5h1.371A6.002 6.002 0 0 1 1 8Z" clip-rule="evenodd" />
				</svg>
			</Button>
		{/if}
		<Button variant="ghost" size="icon-xs" onclick={onremove} disabled={acting} title="Delete">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 text-red-500">
				<path fill-rule="evenodd" d="M5 3.25V4H2.75a.75.75 0 0 0 0 1.5h.3l.815 8.15A1.5 1.5 0 0 0 5.357 15h5.285a1.5 1.5 0 0 0 1.493-1.35l.815-8.15h.3a.75.75 0 0 0 0-1.5H11v-.75A2.25 2.25 0 0 0 8.75 1h-1.5A2.25 2.25 0 0 0 5 3.25Zm2.25-.75a.75.75 0 0 0-.75.75V4h3v-.75a.75.75 0 0 0-.75-.75h-1.5ZM6.05 6a.75.75 0 0 1 .787.713l.275 5.5a.75.75 0 0 1-1.498.075l-.275-5.5A.75.75 0 0 1 6.05 6Zm3.9 0a.75.75 0 0 1 .712.787l-.275 5.5a.75.75 0 0 1-1.498-.075l.275-5.5a.75.75 0 0 1 .786-.711Z" clip-rule="evenodd" />
			</svg>
		</Button>
	</td>
</tr>

{#if expanded}
	<tr class="bg-gray-50/50">
		<td colspan="6" class="px-4 py-4">
			<div class="grid grid-cols-2 gap-x-8 gap-y-4 text-sm">
				<div class="space-y-3">
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Source</div>
						<div class="mt-1 font-mono text-xs text-gray-700">{slot.nzb_name}</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Path</div>
						<div class="mt-1 font-mono text-xs text-gray-700 break-all">{slot.path}</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Repair Summary</div>
						<div class="mt-1 text-gray-700">{slot.url_info || 'N/A'}</div>
					</div>
				</div>
				<div class="space-y-3">
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Download Stats</div>
						<div class="mt-1 text-gray-700">
							Downloaded in {formatDuration(slot.download_time)} at {formatSpeed(slot.bytes, slot.download_time)}
						</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Usenet Information</div>
						<div class="mt-1 text-gray-700">
							{slot.storage} old
						</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500">Servers</div>
						<div class="mt-1 text-gray-700 italic">
							{slot.meta || 'N/A'}
						</div>
					</div>
				</div>
			</div>
		</td>
	</tr>
{/if}
