<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import { postAction } from '$lib/api';
	import { registerShortcuts } from '$lib/shortcuts.svelte';
	import { getTheme, cycleTheme } from '$lib/stores/theme.svelte';
	import { onMount } from 'svelte';
	import {
		ArrowDownCircle,
		Play,
		Pause,
		Sun,
		Moon,
		Monitor,
		Activity,
		Info,
		Server,
		Settings,
		Plus
	} from '@lucide/svelte';
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

<header class="sticky top-0 z-40 border-b border-border/50 bg-card/85 backdrop-blur-md transition-colors">
	<div class="mx-auto flex h-16 max-w-7xl items-center gap-3 px-4">
		<h1 class="text-xl font-bold tracking-tight text-foreground flex items-center gap-2 select-none">
			<ArrowDownCircle class="size-6 text-primary" />
			<a
				href="https://www.github.com/hobeone/gonzbd"
				target="_blank"
				rel="noopener noreferrer"
				class="hover:opacity-85 transition-opacity"
			>
				GoNZBD
			</a>
		</h1>

		<Button
			variant="ghost"
			class="ml-2 gap-2 rounded-full bg-muted/60 px-4 py-2 text-xs font-bold text-foreground hover:bg-primary/15 hover:text-primary active:scale-[0.98] transition-all select-none"
			onclick={togglePause}
			disabled={toggling}
		>
			{#if paused}
				<Play class="size-4 fill-current" />
				Resume
			{:else}
				<Pause class="size-4 fill-current" />
				Pause
			{/if}
		</Button>

		<div class="flex-1"></div>

		<div class="flex items-center gap-1">
			<Button
				variant="ghost"
				size="icon"
				class="size-9 rounded-full text-muted-foreground hover:bg-muted/80 hover:text-foreground active:scale-95 transition-all"
				onclick={cycleTheme}
				title="Theme: {theme} (press D to cycle)"
			>
				{#if theme === 'light'}
					<Sun class="size-4" />
				{:else if theme === 'dark'}
					<Moon class="size-4" />
				{:else}
					<Monitor class="size-4" />
				{/if}
			</Button>

			<Button
				variant="ghost"
				size="icon"
				class="size-9 rounded-full text-muted-foreground hover:bg-muted/80 hover:text-foreground active:scale-95 transition-all"
				href="/status"
				title="Status"
			>
				<Activity class="size-4" />
			</Button>

			<Button
				variant="ghost"
				size="icon"
				class="size-9 rounded-full text-muted-foreground hover:bg-muted/80 hover:text-foreground active:scale-95 transition-all"
				onclick={() => (aboutOpen = true)}
				title="About GoNZBD"
			>
				<Info class="size-4" />
			</Button>

			<Button
				variant="ghost"
				size="icon"
				class="size-9 rounded-full text-muted-foreground hover:bg-muted/80 hover:text-foreground active:scale-95 transition-all"
				onclick={() => (serverStatusOpen = true)}
				title="Server Status"
			>
				<Server class="size-4" />
			</Button>

			<Button
				variant="ghost"
				size="icon"
				class="size-9 rounded-full text-muted-foreground hover:bg-muted/80 hover:text-foreground active:scale-95 transition-all"
				onclick={() => (settingsOpen = true)}
				title="Settings"
			>
				<Settings class="size-4" />
			</Button>
		</div>

		<Button
			variant="default"
			class="gap-1.5 rounded-full bg-primary px-4 py-2 text-xs font-bold text-primary-foreground shadow-sm hover:bg-primary/90 hover:shadow active:scale-[0.98] transition-all"
			onclick={() => (addDialogOpen = true)}
		>
			<Plus class="size-4 stroke-[3]" />
			Add NZB
		</Button>
	</div>
</header>

<AddNzbDialog bind:open={addDialogOpen} />
<SettingsDialog bind:open={settingsOpen} />
<ServerStatusPanel bind:open={serverStatusOpen} />
<AboutDialog bind:open={aboutOpen} />
