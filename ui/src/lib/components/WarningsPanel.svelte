<script lang="ts">
	import { getWarnings, getWarningsError, clearWarnings } from '$lib/stores/warnings.svelte';
	import { Button } from '$lib/components/ui/button';

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

{#if getWarningsError()}
	<div class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
		API error: {getWarningsError()}
	</div>
{:else if getWarnings().length === 0}
	<div class="rounded-lg border bg-white p-8 text-center text-gray-500">
		No warnings
	</div>
{:else}
	<div class="space-y-2">
		<div class="flex items-center justify-between">
			<span class="text-sm text-gray-500">{getWarnings().length} warning{getWarnings().length !== 1 ? 's' : ''}</span>
			<Button variant="outline" size="sm" onclick={handleClear} disabled={clearing}>
				{clearing ? 'Clearing...' : 'Clear all'}
			</Button>
		</div>
		<div class="space-y-1">
			{#each getWarnings() as warning, i}
				<div class="rounded-md border bg-white px-4 py-3 text-sm text-gray-700">
					<span class="mr-2 font-mono text-xs text-gray-400">#{i + 1}</span>
					{warning}
				</div>
			{/each}
		</div>
	</div>
{/if}
