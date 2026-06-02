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

<nav class="flex w-20 flex-col items-center justify-between border-r border-border bg-card text-foreground py-6 shrink-0 h-screen select-none">
	<!-- Top: Brand Logo / Pause Action -->
	<div class="flex flex-col items-center gap-6 w-full">
		<div class="flex h-12 w-12 items-center justify-center rounded-full bg-primary/20 text-primary-foreground font-black text-sm tracking-tighter" title="GoNZBD">
			Go
		</div>

		<!-- Pause/Resume Action Button -->
		<Button
			variant="ghost"
			size="icon"
			class="h-12 w-12 rounded-2xl bg-muted text-foreground hover:bg-muted/80 transition-all shadow-sm"
			onclick={togglePause}
			disabled={toggling}
			title={paused ? 'Resume' : 'Pause'}
			aria-label={paused ? 'Resume' : 'Pause'}
		>
			{#if paused}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-6 text-emerald-500">
					<path d="M6.3 2.84A1.5 1.5 0 0 0 4 4.11v11.78a1.5 1.5 0 0 0 2.3 1.27l9.344-5.891a1.5 1.5 0 0 0 0-2.538L6.3 2.841Z" />
				</svg>
			{:else}
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-6 text-primary">
					<path d="M5.75 3a.75.75 0 0 0-.75.75v12.5c0 .414.336.75.75.75h1.5a.75.75 0 0 0 .75-.75V3.75A.75.75 0 0 0 7.25 3h-1.5ZM12.75 3a.75.75 0 0 0-.75.75v12.5c0 .414.336.75.75.75h1.5a.75.75 0 0 0 .75-.75V3.75a.75.75 0 0 0-.75-.75h-1.5Z" />
				</svg>
			{/if}
		</Button>
	</div>

	<!-- Middle: Navigation Rail Toggles (Add, Settings, Server Status) -->
	<div class="flex flex-col items-center gap-6 w-full">
		<!-- Add NZB Button -->
		<Button
			variant="ghost"
			size="icon"
			class="h-12 w-12 rounded-2xl hover:bg-muted text-muted-foreground hover:text-foreground transition-all"
			onclick={() => (addDialogOpen = true)}
			title="Add NZB (A)"
			aria-label="Add NZB"
		>
			<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-6">
				<path stroke-linecap="round" stroke-linejoin="round" d="M12 4.5v15m7.5-7.5h-15" />
			</svg>
		</Button>

		<!-- Server Status Panel Toggle -->
		<Button
			variant="ghost"
			size="icon"
			class="h-12 w-12 rounded-2xl hover:bg-muted text-muted-foreground hover:text-foreground transition-all"
			onclick={() => (serverStatusOpen = true)}
			title="Server Status (V)"
			aria-label="Server Status"
		>
			<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-6">
				<path stroke-linecap="round" stroke-linejoin="round" d="M3.75 6A2.25 2.25 0 0 1 6 3.75h2.25A2.25 2.25 0 0 1 10.5 6v2.25a2.25 2.25 0 0 1-2.25 2.25H6a2.25 2.25 0 0 1-2.25-2.25V6ZM3.75 15.75A2.25 2.25 0 0 1 6 13.5h2.25a2.25 2.25 0 0 1 2.25 2.25V18a2.25 2.25 0 0 1-2.25 2.25H6A2.25 2.25 0 0 1 3.75 18v-2.25ZM13.5 6a2.25 2.25 0 0 1 2.25-2.25H18A2.25 2.25 0 0 1 20.25 6v2.25A2.25 2.25 0 0 1 18 10.5h-2.25a2.25 2.25 0 0 1-2.25-2.25V6ZM13.5 15.75a2.25 2.25 0 0 1 2.25-2.25H18a2.25 2.25 0 0 1 2.25 2.25V18A2.25 2.25 0 0 1 18 20.25h-2.25A2.25 2.25 0 0 1 13.5 18v-2.25Z" />
			</svg>
		</Button>

		<!-- Settings Gear Toggle -->
		<Button
			variant="ghost"
			size="icon"
			class="h-12 w-12 rounded-2xl hover:bg-muted text-muted-foreground hover:text-foreground transition-all"
			onclick={() => (settingsOpen = true)}
			title="Settings (S)"
			aria-label="Settings"
		>
			<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-6">
				<path stroke-linecap="round" stroke-linejoin="round" d="M9.594 3.94c.09-.542.56-.94 1.11-.94h2.593c.55 0 1.02.398 1.11.94l.213 1.281c.063.374.313.686.645.87.074.04.147.083.22.127.324.196.72.257 1.075.124l1.217-.456a1.125 1.125 0 0 1 1.37.49l1.296 2.247a1.125 1.125 0 0 1-.26 1.43l-1.003.828c-.293.241-.438.613-.43.992a7.723 7.723 0 0 1 0 .255c-.008.378.137.75.43.991l1.004.827c.424.35.534.954.26 1.43l-1.298 2.247a1.125 1.125 0 0 1-1.369.491l-1.217-.456c-.355-.133-.75-.072-1.076.124a6.47 6.47 0 0 1-.22.128c-.331.183-.581.495-.644.869l-.213 1.281c-.09.543-.56.94-1.11.94h-2.594c-.55 0-1.019-.398-1.11-.94l-.213-1.281c-.062-.374-.312-.686-.644-.87a6.52 6.52 0 0 1-.22-.127c-.325-.196-.72-.257-1.076-.124l-1.217.456a1.125 1.125 0 0 1-1.369-.49l-1.297-2.247a1.125 1.125 0 0 1 .26-1.43l1.004-.827c.292-.24.437-.613.43-.991a6.932 6.932 0 0 1 0-.255c.007-.38-.138-.751-.43-.992l-1.004-.827a1.125 1.125 0 0 1-.26-1.43l1.297-2.247a1.125 1.125 0 0 1 1.37-.491l1.216.456c.356.133.751.072 1.076-.124.072-.044.146-.086.22-.128.332-.183.582-.495.644-.869l.214-1.28Z" />
				<path stroke-linecap="round" stroke-linejoin="round" d="M15 12a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z" />
			</svg>
		</Button>
	</div>

	<!-- Bottom: Theme & About Buttons -->
	<div class="flex flex-col items-center gap-4 w-full">
		<!-- Cycle Theme Button -->
		<Button
			variant="ghost"
			size="icon"
			class="h-10 w-10 rounded-2xl hover:bg-muted text-muted-foreground hover:text-foreground transition-all"
			onclick={cycleTheme}
			title="Cycle Theme (D)"
			aria-label="Cycle Theme"
		>
			{#if theme === 'light'}
				<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-5">
					<path stroke-linecap="round" stroke-linejoin="round" d="M12 3v2.25m6.364.386-1.591 1.591M21 12h-2.25m-.386 6.364-1.591-1.591M12 18.75V21m-4.773-4.227-1.591 1.591M5.25 12H3m4.227-4.773L5.636 5.636M15.75 12a3.75 3.75 0 1 1-7.5 0 3.75 3.75 0 0 1 7.5 0Z" />
				</svg>
			{:else if theme === 'dark'}
				<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-5">
					<path stroke-linecap="round" stroke-linejoin="round" d="M21.752 15.002A9.72 9.72 0 0 1 18 15.75c-5.385 0-9.75-4.365-9.75-9.75 0-1.33.266-2.597.748-3.752A9.753 9.753 0 0 0 3 11.25C3 16.635 7.365 21 12.75 21a9.753 9.753 0 0 0 9.002-5.998Z" />
				</svg>
			{:else}
				<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-5">
					<path stroke-linecap="round" stroke-linejoin="round" d="M9 17.25v1.007a3 3 0 0 1-.879 2.122L7.5 21h9l-.621-.621A3 3 0 0 1 15 18.257V17.25m6-12V15a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 15V5.25m18 0A2.25 2.25 0 0 0 18.75 3H5.25A2.25 2.25 0 0 0 3 5.25m18 0V12a2.25 2.25 0 0 1-2.25 2.25H5.25A2.25 2.25 0 0 1 3 12V5.25" />
				</svg>
			{/if}
		</Button>

		<!-- About Information Toggle -->
		<Button
			variant="ghost"
			size="icon"
			class="h-10 w-10 rounded-2xl hover:bg-muted text-muted-foreground hover:text-foreground transition-all"
			onclick={() => (aboutOpen = true)}
			title="About GoNZBD (I)"
			aria-label="About GoNZBD"
		>
			<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" class="size-5">
				<path stroke-linecap="round" stroke-linejoin="round" d="M9.879 7.519c1.171-1.025 3.071-1.025 4.242 0 1.172 1.025 1.172 2.687 0 3.712-.203.179-.43.326-.67.442-.745.361-1.45.999-1.45 1.827v.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Zm-9 5.25h.008v.008H12v-.008Z" />
			</svg>
		</Button>
	</div>
</nav>

<AddNzbDialog bind:open={addDialogOpen} />
<SettingsDialog bind:open={settingsOpen} />
<ServerStatusPanel bind:open={serverStatusOpen} />
<AboutDialog bind:open={aboutOpen} />
