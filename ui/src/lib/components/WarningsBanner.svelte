<script lang="ts">
	import { getWarnings, getWarningsError, getWarningCount, clearWarnings } from '$lib/stores/warnings.svelte';
	import { Button } from '$lib/components/ui/button';

	let expanded = $state(true);
	let clearing = $state(false);

	async function handleClear() {
		clearing = true;
		try {
			await clearWarnings();
		} finally {
			clearing = false;
		}
	}
</script>

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
