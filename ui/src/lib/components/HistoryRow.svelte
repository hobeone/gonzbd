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

	function formatSize(bytes: number): string {
		if (bytes < 1024) return `${bytes} B`;
		if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
		if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
		return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
	}

	// stageLineClass picks the Tailwind classes for a single stage-log line
	// based on a leading marker. Classification is done on the trimStart()ed
	// content so a marker keeps its color even on indented sub-items (e.g.
	// the per-file "  ✓ …"/"  ⚠ …" completion lines, or an indented
	// "old → new" rename); the literal indentation is preserved visually by
	// the div's whitespace-pre-wrap. The plain-indent fallback is checked
	// last so it only catches indented lines with no semantic marker — this
	// is the ordering that previously mis-colored indented ✓/⚠/→ lines gray.
	function stageLineClass(action: string): string {
		const marker = action.trimStart();
		if (marker.startsWith('Error:')) return 'text-red-600 dark:text-red-400 font-medium';
		if (marker.startsWith('⚠')) return 'text-amber-600 dark:text-amber-400 font-medium';
		if (marker.startsWith('✓')) return 'text-green-600 dark:text-green-400 font-medium';
		if (marker.startsWith('Skipped:')) return 'text-yellow-600 dark:text-yellow-400 font-medium';
		if (marker.startsWith('Running:')) return 'font-mono text-blue-600 dark:text-blue-400';
		if (marker.startsWith('Pipeline')) return 'font-semibold text-gray-700 dark:text-gray-300';
		if (marker.includes('→')) return 'text-emerald-600 dark:text-emerald-400';
		if (
			marker.startsWith('Files ') ||
			marker.startsWith('Final ') ||
			marker.startsWith('Downloaded ') ||
			marker.startsWith('Servers:') ||
			marker.startsWith('Total:')
		)
			return 'font-medium text-gray-700 dark:text-gray-300';
		if (action.startsWith('  ') || action.startsWith('-  '))
			return 'font-mono text-gray-600 dark:text-gray-400 pl-2';
		return 'text-gray-500 dark:text-gray-400';
	}
</script>

<tr class="border-b hover:bg-gray-50 dark:hover:bg-gray-800 cursor-pointer text-gray-900 dark:text-gray-100" onclick={toggle}>
	<td class="px-4 py-3 max-w-[200px] sm:max-w-xs md:max-w-sm lg:max-w-md xl:max-w-lg">
		<div class="flex items-center gap-2">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 shrink-0 text-gray-400 transition-transform {expanded ? 'rotate-90' : ''}">
				<path d="M5.75 3a.75.75 0 0 0-.75.75v8.5c0 .414.336.75.75.75h.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-.5ZM10.25 3a.75.75 0 0 0-.75.75v8.5c0 .414.336.75.75.75h.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-.5Z" class="hidden" />
				<path d="M6.22 3.22a.75.75 0 0 1 1.06 0l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 0 1 0-1.06Z" />
			</svg>
			<div class="font-medium truncate" title={slot.name}>{slot.name}</div>
		</div>
		{#if slot.fail_message}
			<div class="ml-6 mt-0.5 text-xs text-red-600 truncate" title={slot.fail_message}>{slot.fail_message}</div>
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
	<tr class="bg-gray-50/50 dark:bg-gray-900/50">
		<td colspan="6" class="px-4 py-4">
			{#if slot.fail_message}
				<div class="mb-4 rounded-md border border-red-200 dark:border-red-800 bg-red-50 dark:bg-red-950/30 px-4 py-3">
					<div class="text-xs font-semibold uppercase tracking-wider text-red-600 dark:text-red-400">Failure Reason</div>
					<div class="mt-1 text-sm text-red-700 dark:text-red-300">{slot.fail_message}</div>
				</div>
			{/if}
			<div class="grid grid-cols-2 gap-x-8 gap-y-4 text-sm">
				<div class="space-y-3">
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Source</div>
						<div class="mt-1 font-mono text-xs text-gray-700 dark:text-gray-300 break-all">{slot.nzb_name}</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Path</div>
						<div class="mt-1 font-mono text-xs text-gray-700 dark:text-gray-300 break-all">{slot.path}</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Download Health</div>
						<div class="mt-1 flex items-center gap-2">
							{#if slot.completeness > 0}
								<div class="flex h-2 w-24 overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700">
									<div
										class="h-full rounded-full transition-all {slot.completeness >= 100 ? 'bg-green-500' : slot.completeness >= 95 ? 'bg-yellow-500' : 'bg-red-500'}"
										style="width: {slot.completeness}%"
									></div>
								</div>
								<span class="text-xs font-medium {slot.completeness >= 100 ? 'text-green-600 dark:text-green-400' : slot.completeness >= 95 ? 'text-yellow-600 dark:text-yellow-400' : 'text-red-600 dark:text-red-400'}">
									{slot.completeness}%
								</span>
							{:else}
								<span class="text-xs text-gray-500 dark:text-gray-400">N/A</span>
							{/if}
						</div>
					</div>
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Repair Summary</div>
						<div class="mt-1 text-gray-700 dark:text-gray-300 break-all">{slot.url_info || 'N/A'}</div>
					</div>
				</div>
				<div class="space-y-3">
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Download Stats</div>
						<div class="mt-1 text-gray-700 dark:text-gray-300">
							Downloaded in {formatDuration(slot.download_time)} at {formatSpeed(slot.downloaded > 0 ? slot.downloaded : slot.bytes, slot.download_time)}
						</div>
						{#if slot.downloaded > 0 && slot.downloaded !== slot.bytes}
							<div class="text-xs text-gray-500 dark:text-gray-400">
								{formatSize(slot.downloaded)} of {formatSize(slot.bytes)} received ({formatSize(slot.bytes - slot.downloaded)} failed)
							</div>
						{/if}
					</div>
					{#if slot.postproc_time > 0}
						<div>
							<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Post-Processing</div>
							<div class="mt-1 text-gray-700 dark:text-gray-300">
								Completed in {formatDuration(slot.postproc_time)}
							</div>
						</div>
					{/if}
					<div>
						<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400">Servers</div>
						<div class="mt-1 text-gray-700 dark:text-gray-300 italic break-all">
							{slot.meta || 'N/A'}
						</div>
					</div>
				</div>
			</div>

			{#if slot.stage_log.length > 0}
				<div class="mt-4 border-t border-gray-200 dark:border-gray-700 pt-3">
					<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400 mb-2">Processing Stages</div>
					<div class="space-y-2">
						{#each slot.stage_log as stage (stage.name)}
							<div class="rounded border border-gray-200 dark:border-gray-700 bg-white dark:bg-gray-800 px-3 py-2">
								<div class="text-xs font-semibold capitalize text-gray-700 dark:text-gray-300">{stage.name}</div>
								{#if stage.actions.length > 0}
									<div class="mt-1 space-y-0.5">
										{#each stage.actions as action, ai (ai)}
											{#if action === ''}
												<div class="h-1"></div>
											{:else}
												<div class="whitespace-pre-wrap text-xs {stageLineClass(action)}">{action}</div>
											{/if}
										{/each}
									</div>
								{/if}
							</div>
						{/each}
					</div>
				</div>
			{/if}
		</td>
	</tr>
{/if}

