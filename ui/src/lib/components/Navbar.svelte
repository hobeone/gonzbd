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

<nav class="border-b border-m3-outline/20 bg-m3-surface text-m3-on-surface">
	<div class="mx-auto flex h-16 max-w-7xl items-center gap-3 px-4">
		<h1 class="text-xl font-medium tracking-tight text-m3-primary flex items-center gap-2 select-none">
			<span class="material-symbols-outlined text-2xl font-bold">download_for_offline</span>
      <a href="https://www.github.com/hobeone/gonzbd" target="_blank" rel="noopener noreferrer">GoNZBD</a>
		</h1>

		<Button
			variant="ghost"
			class="nav-chip"
			onclick={togglePause}
			disabled={toggling}
		>
			{#if paused}
				<span class="material-symbols-outlined text-lg leading-none">play_arrow</span>
				Resume
			{:else}
				<span class="material-symbols-outlined text-lg leading-none">pause</span>
				Pause
			{/if}
		</Button>

		<div class="flex-1"></div>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-m3-on-surface-variant hover:bg-m3-surface-variant/50 hover:text-m3-on-surface rounded-full transition-all"
			onclick={cycleTheme}
			title="Theme: {theme} (press D to cycle)"
		>
			{#if theme === 'light'}
				<span class="material-symbols-outlined text-lg">light_mode</span>
			{:else if theme === 'dark'}
				<span class="material-symbols-outlined text-lg">dark_mode</span>
			{:else}
				<span class="material-symbols-outlined text-lg">settings_suggest</span>
			{/if}
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-m3-on-surface-variant hover:bg-m3-surface-variant/50 hover:text-m3-on-surface rounded-full transition-all"
			href="/status"
			title="Status"
		>
			<span class="material-symbols-outlined text-lg">monitor_heart</span>
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-m3-on-surface-variant hover:bg-m3-surface-variant/50 hover:text-m3-on-surface rounded-full transition-all"
			onclick={() => (aboutOpen = true)}
			title="About GoNZBD"
		>
			<span class="material-symbols-outlined text-lg">info</span>
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-m3-on-surface-variant hover:bg-m3-surface-variant/50 hover:text-m3-on-surface rounded-full transition-all"
			onclick={() => (serverStatusOpen = true)}
			title="Server Status"
		>
			<span class="material-symbols-outlined text-lg">dns</span>
		</Button>

		<Button
			variant="ghost"
			size="icon-sm"
			class="text-m3-on-surface-variant hover:bg-m3-surface-variant/50 hover:text-m3-on-surface rounded-full transition-all"
			onclick={() => (settingsOpen = true)}
			title="Settings"
		>
			<span class="material-symbols-outlined text-lg">settings</span>
		</Button>

		<Button
			variant="ghost"
			class="nav-chip bg-m3-primary-container text-m3-on-primary-container hover:bg-m3-primary/20 hover:text-m3-primary"
			onclick={() => (addDialogOpen = true)}
		>
			<span class="material-symbols-outlined text-lg leading-none font-bold">add</span>
			Add NZB
		</Button>
	</div>
</nav>

<AddNzbDialog bind:open={addDialogOpen} />
<SettingsDialog bind:open={settingsOpen} />
<ServerStatusPanel bind:open={serverStatusOpen} />
<AboutDialog bind:open={aboutOpen} />
