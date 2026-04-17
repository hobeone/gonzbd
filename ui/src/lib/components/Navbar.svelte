<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import { postAction } from '$lib/api';
	import { getApiKey } from '$lib/stores/apikey.svelte';

	let {
		paused = false,
		speed = '--',
		onpausetoggle
	}: {
		paused?: boolean;
		speed?: string;
		onpausetoggle?: () => void;
	} = $props();

	let toggling = $state(false);

	async function togglePause() {
		toggling = true;
		try {
			await postAction(getApiKey(), paused ? 'resume' : 'pause');
			onpausetoggle?.();
		} finally {
			toggling = false;
		}
	}
</script>

<nav class="border-b bg-gray-900 text-white">
	<div class="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4">
		<h1 class="text-lg font-bold tracking-tight">SABnzbd</h1>

		<div class="flex items-center gap-2 rounded-md bg-gray-800 px-3 py-1.5 text-sm">
			<span class="text-gray-400">Speed</span>
			<span class="font-mono font-medium">{speed}</span>
		</div>

		<Button
			variant="ghost"
			size="sm"
			class="text-white hover:bg-gray-800"
			onclick={togglePause}
			disabled={toggling}
		>
			{#if paused}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path d="M6.3 2.84A1.5 1.5 0 0 0 4 4.11v11.78a1.5 1.5 0 0 0 2.3 1.27l9.344-5.891a1.5 1.5 0 0 0 0-2.538L6.3 2.841Z" />
				</svg>
				Resume
			{:else}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path d="M5.75 3a.75.75 0 0 0-.75.75v12.5c0 .414.336.75.75.75h1.5a.75.75 0 0 0 .75-.75V3.75A.75.75 0 0 0 7.25 3h-1.5ZM12.75 3a.75.75 0 0 0-.75.75v12.5c0 .414.336.75.75.75h1.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-1.5Z" />
				</svg>
				Pause
			{/if}
		</Button>

		<div class="flex-1"></div>

		<Button
			variant="outline"
			size="sm"
			class="border-gray-700 text-white hover:bg-gray-800"
			disabled
		>
			+ Add NZB
		</Button>
	</div>
</nav>
