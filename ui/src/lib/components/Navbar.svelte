<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import { postAction } from '$lib/api';
	import { registerShortcuts } from '$lib/shortcuts.svelte';
	import { getTheme, cycleTheme } from '$lib/stores/theme.svelte';
	import { onMount } from 'svelte';
	import AddNzbDialog from './AddNzbDialog.svelte';
	import SettingsDialog from './SettingsDialog.svelte';
	import ServerStatusPanel from './ServerStatusPanel.svelte';
	import AboutDialog from './AboutDialog.svelte';

	let {
		paused = false,
		onpausetoggle
	}: {
		paused?: boolean;
		onpausetoggle?: () => void;
	} = $props();

	let toggling = $state(false);
	let addDialogOpen = $state(false);
	let settingsOpen = $state(false);
	let serverStatusOpen = $state(false);
	let aboutOpen = $state(false);

	let theme = $derived(getTheme());

	async function togglePause() {
		toggling = true;
		try {
			await postAction(paused ? 'resume' : 'pause');
			onpausetoggle?.();
		} finally {
			toggling = false;
		}
	}

	onMount(() => {
		return registerShortcuts([
			{ key: ' ', description: 'Pause / Resume', action: togglePause },
			{ key: 'a', description: 'Add NZB', action: () => (addDialogOpen = !addDialogOpen) },
			{ key: 'n', mod: 'ctrl', description: 'Add NZB', action: () => (addDialogOpen = !addDialogOpen) },
			{ key: 's', description: 'Settings', action: () => (settingsOpen = !settingsOpen) },
			{ key: 'v', description: 'Server Status', action: () => (serverStatusOpen = !serverStatusOpen) },
			{ key: 'i', description: 'About', action: () => (aboutOpen = !aboutOpen) },
			{ key: 'd', description: 'Toggle Theme', action: cycleTheme }
		]);
	});
</script>

<nav class="border-b bg-gray-900 text-white">
	<div class="mx-auto flex h-14 max-w-7xl items-center gap-4 px-4">
		<h1 class="text-lg font-bold tracking-tight">GoNZBD</h1>

		<Button
			variant="ghost"
			class="nav-chip"
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
			onclick={cycleTheme}
			title="Theme: {theme} (press D to cycle)"
		>
			{#if theme === 'light'}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path d="M10 2a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 10 2ZM10 15a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 10 15ZM10 7a3 3 0 1 0 0 6 3 3 0 0 0 0-6ZM15.657 5.404a.75.75 0 1 0-1.06-1.06l-1.061 1.06a.75.75 0 0 0 1.06 1.061l1.06-1.06ZM6.464 14.596a.75.75 0 1 0-1.06-1.06l-1.06 1.06a.75.75 0 0 0 1.06 1.06l1.06-1.06ZM18 10a.75.75 0 0 1-.75.75h-1.5a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 18 10ZM5 10a.75.75 0 0 1-.75.75h-1.5a.75.75 0 0 1 0-1.5h1.5A.75.75 0 0 1 5 10ZM14.596 15.657a.75.75 0 0 0 1.06-1.06l-1.06-1.061a.75.75 0 1 0-1.06 1.06l1.06 1.06ZM5.404 6.464a.75.75 0 0 0 1.06-1.06l-1.06-1.06a.75.75 0 1 0-1.06 1.06l1.06 1.06Z" />
				</svg>
			{:else if theme === 'dark'}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path fill-rule="evenodd" d="M7.455 2.004a.75.75 0 0 1 .26.77 7 7 0 0 0 9.958 7.967.75.75 0 0 1 1.067.853A8.5 8.5 0 1 1 6.647 1.921a.75.75 0 0 1 .808.083Z" clip-rule="evenodd" />
				</svg>
			{:else}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path fill-rule="evenodd" d="M2 4.25A2.25 2.25 0 0 1 4.25 2h11.5A2.25 2.25 0 0 1 18 4.25v8.5A2.25 2.25 0 0 1 15.75 15h-3.105a3.501 3.501 0 0 0 1.1 1.677A.75.75 0 0 1 13.26 18H6.74a.75.75 0 0 1-.484-1.323A3.501 3.501 0 0 0 7.355 15H4.25A2.25 2.25 0 0 1 2 12.75v-8.5Zm1.5 0a.75.75 0 0 1 .75-.75h11.5a.75.75 0 0 1 .75.75v7.5a.75.75 0 0 1-.75.75H4.25a.75.75 0 0 1-.75-.75v-7.5Z" clip-rule="evenodd" />
				</svg>
			{/if}
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-gray-400 hover:bg-gray-800 hover:text-white"
			onclick={() => (aboutOpen = true)}
			title="About GoNZBD"
		>
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
				<path fill-rule="evenodd" d="M18 10a8 8 0 1 1-16 0 8 8 0 0 1 16 0Zm-7-4a1 1 0 1 1-2 0 1 1 0 0 1 2 0ZM9 9a.75.75 0 0 0 0 1.5h.253a.25.25 0 0 1 .244.304l-.459 2.066A1.75 1.75 0 0 0 10.747 15H11a.75.75 0 0 0 0-1.5h-.253a.25.25 0 0 1-.244-.304l.459-2.066A1.75 1.75 0 0 0 9.253 9H9Z" clip-rule="evenodd" />
			</svg>
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-gray-400 hover:bg-gray-800 hover:text-white"
			onclick={() => (serverStatusOpen = true)}
			title="Server Status"
		>
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
				<path d="M1 4.75C1 3.784 1.784 3 2.75 3h14.5c.966 0 1.75.784 1.75 1.75v10.515a1.75 1.75 0 0 1-1.75 1.75h-1.5c-.078 0-.155-.005-.23-.015H4.48c-.075.01-.152.015-.23.015h-1.5A1.75 1.75 0 0 1 1 15.265V4.75Zm1.5 0v2.5h15v-2.5a.25.25 0 0 0-.25-.25H2.75a.25.25 0 0 0-.25.25Zm0 4v5.5h2v-5.5h-2Zm3.5 0v5.5h4v-5.5H6Zm5.5 0v5.5h4v-5.5h-4Z" />
			</svg>
		</Button>

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
<ServerStatusPanel bind:open={serverStatusOpen} />
<AboutDialog bind:open={aboutOpen} />
