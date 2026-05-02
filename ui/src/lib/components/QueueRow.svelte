<script lang="ts">
	import type { QueueSlot } from '$lib/types';
	import { Progress } from '$lib/components/ui/progress';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';
	import { cn, formatSize as formatBytes } from '$lib/utils';

	let { slot, onremove }: { slot: QueueSlot; onremove: () => void } = $props();

	let acting = $state(false);
	let expanded = $state(false);

	let percentage = $derived(parseFloat(slot.percentage) || 0);
	let isPaused = $derived(slot.status === 'Paused');
	let isPostProc = $derived(
		['Verifying', 'Repairing', 'Extracting', 'Moving', 'Running'].includes(slot.status)
	);
	let isActive = $derived(
		slot.status !== 'Queued' && slot.status !== 'Paused' && slot.status !== 'Idle'
	);
	let hasFailed = $derived(slot.failed_bytes > 0);

	async function togglePause() {
		acting = true;
		try {
			if (isPaused) {
				await resumeJob(slot.nzo_id);
			} else {
				await pauseJob(slot.nzo_id);
			}
		} finally {
			acting = false;
		}
	}

	/** Health indicator: can par2 cover the damage? */
	function healthLabel(): { text: string; color: string } {
		if (slot.failed_bytes === 0) return { text: 'Healthy', color: 'text-emerald-600 dark:text-emerald-400' };
		if (slot.par2_bytes === 0) return { text: 'No repair data', color: 'text-red-600 dark:text-red-400' };
		if (slot.failed_bytes <= slot.par2_bytes) return { text: 'Repairable', color: 'text-amber-600 dark:text-amber-400' };
		return { text: 'Beyond repair', color: 'text-red-600 dark:text-red-400' };
	}
</script>

<tr
	class="border-b hover:bg-gray-50 dark:hover:bg-gray-800 text-gray-900 dark:text-gray-100 cursor-pointer"
	onclick={() => { expanded = !expanded; }}
>
	<td class="px-4 py-3 max-w-[200px] sm:max-w-xs md:max-w-sm lg:max-w-md xl:max-w-lg">
		<div class="flex items-center gap-2">
			<svg
				xmlns="http://www.w3.org/2000/svg"
				viewBox="0 0 16 16"
				fill="currentColor"
				class={cn('size-4 shrink-0 transition-transform', expanded && 'rotate-90')}
			>
				<path
					fill-rule="evenodd"
					d="M6.22 4.22a.75.75 0 0 1 1.06 0l3.25 3.25a.75.75 0 0 1 0 1.06l-3.25 3.25a.75.75 0 0 1-1.06-1.06L8.94 8 6.22 5.28a.75.75 0 0 1 0-1.06Z"
					clip-rule="evenodd"
				/>
			</svg>
			<div class="font-medium truncate" title={slot.name || slot.filename}>
				{slot.name || slot.filename}
			</div>
			{#if slot.warning}
				<div class="flex items-center text-amber-600 shrink-0 max-w-[100px]" title={slot.warning}>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 shrink-0">
						<path
							fill-rule="evenodd"
							d="M6.701 2.25c.577-1 1.419-1 1.998 0l5.156 8.93c.577 1 .158 1.82-1 1.82H3.145c-1.158 0-1.577-.82-1-1.82l5.156-8.93ZM8 5.5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 5.5Zm0 6a.625.625 0 1 0 0-1.25.625.625 0 0 0 0 1.25Z"
							clip-rule="evenodd"
						/>
					</svg>
					<span class="ml-1 text-xs font-semibold truncate">{slot.warning}</span>
				</div>
			{/if}
			{#if hasFailed}
				<span class="shrink-0 text-xs font-medium text-red-500 dark:text-red-400" title="Failed download bytes">
					✗ {formatBytes(slot.failed_bytes)}
				</span>
			{/if}
		</div>
	</td>
	<td class="px-4 py-3">
		<div class="flex w-32 items-center gap-2">
			<Progress
				value={percentage}
				max={100}
				class={cn('h-2 flex-1', isActive && 'animate-pulse')}
			/>
			<span class="text-xs font-mono text-gray-500 dark:text-gray-400 w-9 text-right">{slot.percentage}%</span>
		</div>
	</td>
	<td class="px-4 py-3 text-sm whitespace-nowrap">{slot.size}</td>
	<td class="px-4 py-3 text-sm whitespace-nowrap">{slot.sizeleft}</td>
	<td class="px-4 py-3">
		<Badge variant={isPaused ? 'outline' : isPostProc ? 'secondary' : 'default'} class="text-xs">
			{slot.status}
		</Badge>
	</td>
	<td class="px-4 py-3 text-sm">{slot.category || '*'}</td>
	<td class="px-4 py-3">
		<div class="flex gap-1">
			<!-- svelte-ignore a11y_click_events_have_key_events -->
			<Button
				variant="ghost"
				size="icon-xs"
				onclick={(e: MouseEvent) => { e.stopPropagation(); togglePause(); }}
				disabled={acting}
				title={isPaused ? 'Resume' : 'Pause'}
			>
				{#if isPaused}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path
							d="M4.5 2A1.5 1.5 0 0 0 3 3.5v9a1.5 1.5 0 0 0 2.3 1.27l7-4.5a1.5 1.5 0 0 0 0-2.54l-7-4.5A1.5 1.5 0 0 0 4.5 2Z"
						/>
					</svg>
				{:else}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path
							d="M4.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75A.75.75 0 0 0 5.5 2h-1ZM10.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75a.75.75 0 0 0-.75-.75h-1Z"
						/>
					</svg>
				{/if}
			</Button>

			<Button variant="ghost" size="icon-xs" onclick={(e: MouseEvent) => { e.stopPropagation(); onremove(); }} disabled={acting} title="Delete">
				<svg
					xmlns="http://www.w3.org/2000/svg"
					viewBox="0 0 16 16"
					fill="currentColor"
					class="size-3.5 text-red-500"
				>
					<path
						fill-rule="evenodd"
						d="M5 3.25V4H2.75a.75.75 0 0 0 0 1.5h.3l.815 8.15A1.5 1.5 0 0 0 5.357 15h5.285a1.5 1.5 0 0 0 1.493-1.35l.815-8.15h.3a.75.75 0 0 0 0-1.5H11v-.75A2.25 2.25 0 0 0 8.75 1h-1.5A2.25 2.25 0 0 0 5 3.25Zm2.25-.75a.75.75 0 0 0-.75.75V4h3v-.75a.75.75 0 0 0-.75-.75h-1.5ZM6.05 6a.75.75 0 0 1 .787.713l.275 5.5a.75.75 0 0 1-1.498.075l-.275-5.5A.75.75 0 0 1 6.05 6Zm3.9 0a.75.75 0 0 1 .712.787l-.275 5.5a.75.75 0 0 1-1.498-.075l.275-5.5a.75.75 0 0 1 .786-.711Z"
						clip-rule="evenodd"
					/>
				</svg>
			</Button>
		</div>
	</td>
</tr>

{#if expanded}
	<tr class="border-b bg-gray-50/50 dark:bg-gray-800/50">
		<td colspan="7" class="px-6 py-3">
			<div class="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Failed</span>
					{#if slot.failed_bytes > 0}
						<div class="font-medium text-red-600 dark:text-red-400">{formatBytes(slot.failed_bytes)}</div>
					{:else}
						<div class="font-medium text-emerald-600 dark:text-emerald-400">None</div>
					{/if}
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Par2 Recovery</span>
					<div class="font-medium">
						{#if slot.par2_bytes > 0}
							{formatBytes(slot.par2_bytes)}
							<span class="text-gray-400 text-xs">({slot.par2_files} file{slot.par2_files !== 1 ? 's' : ''})</span>
						{:else}
							<span class="text-gray-400">None</span>
						{/if}
					</div>
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Health</span>
					<div class={cn('font-medium', healthLabel().color)}>{healthLabel().text}</div>
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Downloaded</span>
					<div class="font-medium">{formatBytes(slot.bytes - slot.remaining_bytes)} of {slot.size}</div>
				</div>
			</div>
		</td>
	</tr>
{/if}
