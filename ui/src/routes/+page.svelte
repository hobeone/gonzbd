<script lang="ts">
	import Navbar from '$lib/components/Navbar.svelte';
	import QueueTable from '$lib/components/QueueTable.svelte';
	import HistoryTable from '$lib/components/HistoryTable.svelte';
	import WarningsBanner from '$lib/components/WarningsBanner.svelte';
	import StatusBar from '$lib/components/StatusBar.svelte';
	import Toast from '$lib/components/Toast.svelte';
	import ConnectionOverlay from '$lib/components/ConnectionOverlay.svelte';
	import ShortcutHelp from '$lib/components/ShortcutHelp.svelte';
	import { onMount, onDestroy } from 'svelte';
	import { startPolling, stopPolling, isPaused, getQueueSlots, getSpeedBytesPerSec } from '$lib/stores/queue.svelte';
	import { startHistoryPolling, stopHistoryPolling } from '$lib/stores/history.svelte';
	import { startWarningsPolling, stopWarningsPolling } from '$lib/stores/warnings.svelte';
	import { faviconForState, type AppState } from '$lib/favicon';
	import { handleGlobalShortcut, registerShortcut } from '$lib/shortcuts.svelte';

	let helpOpen = $state(false);

	onMount(() => {
		startPolling();
		startHistoryPolling();
		startWarningsPolling();

		return registerShortcut({
			key: '?',
			description: 'Show keyboard shortcuts',
			action: () => (helpOpen = !helpOpen)
		});
	});

	onDestroy(() => {
		stopPolling();
		stopHistoryPolling();
		stopWarningsPolling();
	});

	const appState = $derived.by((): AppState => {
		if (isPaused()) return 'paused';
		if (getSpeedBytesPerSec() > 0 || getQueueSlots().some(s => s.status === 'Downloading')) return 'downloading';
		return 'idle';
	});

	const titleEmoji = $derived(
		appState === 'downloading' ? '⬇' : appState === 'paused' ? '⏸' : '●'
	);

	const faviconHref = $derived(faviconForState(appState));
</script>

<svelte:head>
	<title>{titleEmoji} {getQueueSlots().length} item{getQueueSlots().length !== 1 ? 's' : ''} | GoNZBD</title>
	<link rel="icon" type="image/svg+xml" href={faviconHref} />
</svelte:head>

<svelte:window onkeydown={handleGlobalShortcut} />

<div class="flex min-h-screen flex-col bg-background text-foreground antialiased selection:bg-primary/20 selection:text-primary">
	<Navbar paused={isPaused()} onpausetoggle={() => {}} />
	<StatusBar />
	<ConnectionOverlay />

	<main class="mx-auto w-full max-w-7xl flex-1 space-y-8 px-4 pt-6 pb-12">
		<WarningsBanner />

		<section class="space-y-3">
			<div class="flex items-center gap-3">
				<h2 class="text-xs font-bold uppercase tracking-wider text-muted-foreground/80">Queue</h2>
				<div class="h-px flex-1 bg-border/40"></div>
			</div>
			<QueueTable />
		</section>

		<section class="space-y-3">
			<div class="flex items-center gap-3">
				<h2 class="text-xs font-bold uppercase tracking-wider text-muted-foreground/80">History</h2>
				<div class="h-px flex-1 bg-border/40"></div>
			</div>
			<HistoryTable />
		</section>
	</main>
</div>

<Toast />
<ShortcutHelp bind:open={helpOpen} />

