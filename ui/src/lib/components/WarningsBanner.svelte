<script lang="ts">
	import { getWarnings, getWarningsError, getWarningCount, clearWarnings } from '$lib/stores/warnings.svelte';
	import { getQueueSlots } from '$lib/stores/queue.svelte';
	import { Button } from '$lib/components/ui/button';

	let expanded = $state(true);
	let clearing = $state(false);

	function duplicateCount(): number {
		return getQueueSlots().filter((s) => s.warning === 'Duplicate NZB').length;
	}

	async function handleClear() {
		clearing = true;
		try {
			await clearWarnings();
		} finally {
			clearing = false;
		}
	}
</script>

{#if duplicateCount() > 0}
	<div class="mb-4 flex items-center gap-3 rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-amber-800 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-200">
		<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-5 shrink-0">
			<path fill-rule="evenodd" d="M6.701 2.25c.577-1 1.419-1 1.998 0l5.156 8.93c.577 1 .158 1.82-1 1.82H3.145c-1.158 0-1.577-.82-1-1.82l5.156-8.93ZM8 5.5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 5.5Zm0 6a.625.625 0 1 0 0-1.25.625.625 0 0 0 0 1.25Z" clip-rule="evenodd" />
		</svg>
		<div class="flex-1 text-sm">
			<span class="font-bold">Duplicate NZBs found:</span>
			{duplicateCount()} job{duplicateCount() !== 1 ? 's' : ''} added in paused state.
		</div>
	</div>
{/if}

{#if getWarningCount() > 0 || getWarningsError()}
	<div class="rounded-lg border border-amber-200 bg-amber-50 dark:border-amber-800 dark:bg-amber-950">
		<div class="flex items-center justify-between px-4 py-2">
			<span class="text-sm font-medium text-amber-800 dark:text-amber-200">
				⚠ {getWarningCount()} warning{getWarningCount() !== 1 ? 's' : ''}
			</span>
			<div class="flex items-center gap-2">
				<Button variant="outline" size="sm" onclick={handleClear} disabled={clearing} class="h-7 text-xs">
					{clearing ? 'Clearing...' : 'Clear all'}
				</Button>
				<button
					onclick={() => (expanded = !expanded)}
					class="rounded p-1 text-amber-600 hover:bg-amber-100 dark:text-amber-400 dark:hover:bg-amber-900"
					aria-label={expanded ? 'Collapse warnings' : 'Expand warnings'}
				>
					{expanded ? '▲' : '▼'}
				</button>
			</div>
		</div>
		{#if expanded}
			<div class="border-t border-amber-200 px-4 py-2 dark:border-amber-800">
				{#if getWarningsError()}
					<p class="text-sm text-red-600 dark:text-red-400">API error: {getWarningsError()}</p>
				{:else}
					<ul class="space-y-1">
						{#each getWarnings() as warning, i}
							<li class="text-sm text-amber-800 dark:text-amber-200">
								<span class="mr-2 font-mono text-xs text-amber-500">#{i + 1}</span>{warning}
							</li>
						{/each}
					</ul>
				{/if}
			</div>
		{/if}
	</div>
{/if}
