<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import { postAction } from '$lib/api';
	import AddNzbDialog from './AddNzbDialog.svelte';
	import SettingsDialog from './SettingsDialog.svelte';

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
	let addDialogOpen = $state(false);
	let settingsOpen = $state(false);

	async function togglePause() {
		toggling = true;
		try {
			await postAction(paused ? 'resume' : 'pause');
			onpausetoggle?.();
		} finally {
			toggling = false;
		}
	}
</script>

<nav class="border-b bg-gray-900 text-white">
	<div class="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4">
		<h1 class="text-lg font-bold tracking-tight">SABnzbd</h1>

		<div class="nav-chip">
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
			variant="ghost"
			size="icon-sm"
			class="text-gray-400 hover:bg-gray-800 hover:text-white"
			onclick={() => (settingsOpen = true)}
			title="Settings"
		>
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
				<path fill-rule="evenodd" d="M7.84 1.804A1 1 0 0 1 8.82 1h2.36a1 1 0 0 1 .98.804l.331 1.652a6.993 6.993 0 0 1 1.929 1.115l1.598-.54a1 1 0 0 1 1.186.447l1.18 2.044a1 1 0 0 1-.205 1.251l-1.267 1.113a7.047 7.047 0 0 1 0 2.228l1.267 1.113a1 1 0 0 1 .206 1.25l-1.18 2.045a1 1 0 0 1-1.187.447l-1.598-.54a6.993 6.993 0 0 1-1.929 1.115l-.33 1.652a1 1 0 0 1-.98.804H8.82a1 1 0 0 1-.98-.804l-.331-1.652a6.993 6.993 0 0 1-1.929-1.115l-1.598.54a1 1 0 0 1-1.186-.447l-1.18-2.044a1 1 0 0 1 .205-1.251l1.267-1.114a7.05 7.05 0 0 1 0-2.227L1.821 7.773a1 1 0 0 1-.206-1.25l1.18-2.045a1 1 0 0 1 1.187-.447l1.598.54A6.992 6.992 0 0 1 7.51 3.456l.33-1.652ZM10 13a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z" clip-rule="evenodd" />
			</svg>
		</Button>

		<Button
			variant="ghost"
			class="nav-chip"
			onclick={() => (addDialogOpen = true)}
		>
			+ Add NZB
		</Button>
	</div>
</nav>

<AddNzbDialog bind:open={addDialogOpen} />
<SettingsDialog bind:open={settingsOpen} />
