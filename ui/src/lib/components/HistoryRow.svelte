<script lang="ts">
	import type { HistorySlot } from '$lib/types';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { retryHistoryJob } from '$lib/stores/history.svelte';
	import { showToast } from '$lib/stores/warnings.svelte';
	import { ChevronRight, RotateCcw, Trash2 } from '@lucide/svelte';

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

	function stageLineClass(action: string): string {
		const marker = action.trimStart();
		if (marker.startsWith('Error:')) return 'text-red-600 dark:text-red-400 font-medium';
		if (marker.startsWith('⚠')) return 'text-amber-600 dark:text-amber-400 font-medium';
		if (marker.startsWith('✓')) return 'text-green-600 dark:text-green-400 font-medium';
		if (marker.startsWith('Skipped:')) return 'text-yellow-600 dark:text-yellow-400 font-medium';
		if (marker.startsWith('Running:')) return 'font-mono text-blue-600 dark:text-blue-400';
		if (marker.startsWith('Pipeline')) return 'font-semibold text-foreground';
		if (marker.includes('→')) return 'text-emerald-600 dark:text-emerald-400';
		if (
			marker.startsWith('Files ') ||
			marker.startsWith('Final ') ||
			marker.startsWith('Downloaded ') ||
			marker.startsWith('Servers:') ||
			marker.startsWith('Total:')
		)
			return 'font-medium text-foreground';
		if (action.startsWith('  ') || action.startsWith('-  '))
			return 'font-mono text-gray-600 dark:text-gray-400 pl-2';
		return 'text-muted-foreground';
	}
</script>

<tr class="border-b border-border/40 hover:bg-muted/30 text-foreground cursor-pointer transition-colors" onclick={toggle}>
	<td class="px-5 py-3.5 max-w-[200px] sm:max-w-xs md:max-w-sm lg:max-w-md xl:max-w-lg">
		<div class="flex items-center gap-2">
			<ChevronRight class="size-4 text-muted-foreground transition-transform duration-200 {expanded ? 'rotate-90' : ''} select-none" />
			<div class="font-semibold truncate text-sm text-foreground" title={slot.name}>{slot.name}</div>
		</div>
		{#if slot.fail_message}
			<div class="ml-6 mt-0.5 text-xs text-destructive truncate font-semibold" title={slot.fail_message}>{slot.fail_message}</div>
		{/if}
	</td>
	<td class="px-5 py-3.5 text-xs font-mono font-medium whitespace-nowrap tabular-nums">{slot.size}</td>
	<td class="px-5 py-3.5">
		<Badge variant={statusVariant()} class="text-[11px] font-semibold px-2 py-0.5 rounded-full">
			{slot.status}
		</Badge>
	</td>
	<td class="px-5 py-3.5 text-xs font-semibold">{slot.category || '*'}</td>
	<td class="px-5 py-3.5 text-xs font-mono font-medium whitespace-nowrap tabular-nums">{completedDate()}</td>
	<td class="px-5 py-3.5">
		<div class="flex gap-1 justify-end">
			{#if slot.status === 'Failed'}
				<Button
					variant="ghost"
					size="icon-xs"
					onclick={(e) => { e.stopPropagation(); retry(); }}
					disabled={acting}
					class="rounded-full text-primary hover:bg-primary/10 transition-colors"
					title="Retry"
				>
					<RotateCcw class="size-3.5" />
				</Button>
			{/if}
			<Button
				variant="ghost"
				size="icon-xs"
				onclick={(e) => { e.stopPropagation(); onremove(); }}
				disabled={acting}
				class="rounded-full text-destructive hover:bg-destructive/10 transition-colors"
				title="Delete"
			>
				<Trash2 class="size-3.5" />
			</Button>
		</div>
	</td>
</tr>

{#if expanded}
	<tr class="border-b border-border/40 bg-muted/20 text-foreground">
		<td colspan="6" class="px-6 py-4">
			{#if slot.fail_message}
				<div class="mb-4 rounded-2xl border border-destructive/30 bg-destructive/10 px-4 py-3">
					<div class="text-[10px] font-bold uppercase tracking-wider text-destructive">Failure Reason</div>
					<div class="mt-1 text-xs text-destructive font-semibold">{slot.fail_message}</div>
				</div>
			{/if}
			<div class="grid grid-cols-2 gap-x-8 gap-y-4 text-xs">
				<div class="space-y-3">
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Source</div>
						<div class="mt-1 font-mono text-xs text-foreground break-all font-medium">{slot.nzb_name}</div>
					</div>
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Path</div>
						<div class="mt-1 font-mono text-xs text-foreground break-all font-medium">{slot.path}</div>
					</div>
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Download Health</div>
						<div class="mt-1 flex items-center gap-2">
							{#if slot.completeness > 0}
								<div class="flex h-2 w-24 overflow-hidden rounded-full bg-muted">
									<div
										class="h-full rounded-full transition-all {slot.completeness >= 100 ? 'bg-emerald-500' : slot.completeness >= 95 ? 'bg-amber-500' : 'bg-red-500'}"
										style="width: {slot.completeness}%"
									></div>
								</div>
								<span class="text-xs font-mono font-semibold {slot.completeness >= 100 ? 'text-emerald-500' : slot.completeness >= 95 ? 'text-amber-500' : 'text-red-500'}">
									{slot.completeness}%
								</span>
							{:else}
								<span class="text-xs text-muted-foreground font-semibold">N/A</span>
							{/if}
						</div>
					</div>
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Repair Summary</div>
						<div class="mt-1 text-foreground break-all font-medium">{slot.url_info || 'N/A'}</div>
					</div>
				</div>
				<div class="space-y-3">
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Download Stats</div>
						<div class="mt-1 text-foreground font-medium font-mono">
							Downloaded in {formatDuration(slot.download_time)} at {formatSpeed(slot.downloaded > 0 ? slot.downloaded : slot.bytes, slot.download_time)}
						</div>
						{#if slot.downloaded > 0 && slot.downloaded !== slot.bytes}
							<div class="text-xs text-muted-foreground font-semibold font-mono mt-0.5">
								{formatSize(slot.downloaded)} of {formatSize(slot.bytes)} received ({formatSize(slot.bytes - slot.downloaded)} failed)
							</div>
						{/if}
					</div>
					{#if slot.postproc_time > 0}
						<div>
							<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Post-Processing</div>
							<div class="mt-1 text-foreground font-medium font-mono">
								Completed in {formatDuration(slot.postproc_time)}
							</div>
						</div>
					{/if}
					<div>
						<div class="text-[10px] font-bold uppercase tracking-wider text-primary">Servers</div>
						<div class="mt-1 text-foreground italic break-all font-medium">
							{slot.meta || 'N/A'}
						</div>
					</div>
				</div>
			</div>

			{#if slot.stage_log.length > 0}
				<div class="mt-4 border-t border-border/40 pt-4">
					<div class="text-[10px] font-bold uppercase tracking-wider text-primary mb-3">Processing Stages</div>
					<div class="space-y-2">
						{#each slot.stage_log as stage (stage.name)}
							<div class="rounded-2xl border border-border/40 bg-card p-3.5">
								<div class="text-xs font-bold capitalize text-primary">{stage.name}</div>
								{#if stage.actions.length > 0}
									<div class="mt-2 space-y-0.5">
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
