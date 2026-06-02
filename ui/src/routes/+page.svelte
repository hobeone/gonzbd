<script lang="ts">
	import SidebarNav from '$lib/components/SidebarNav.svelte';
	import StatCard from '$lib/components/StatCard.svelte';
	import QueueTable from '$lib/components/QueueTable.svelte';
	import HistoryTable from '$lib/components/HistoryTable.svelte';
	import WarningsBanner from '$lib/components/WarningsBanner.svelte';
	import Toast from '$lib/components/Toast.svelte';
	import SpeedLimitControl from '$lib/components/SpeedLimitControl.svelte';
	import ConnectionOverlay from '$lib/components/ConnectionOverlay.svelte';
	import ShortcutHelp from '$lib/components/ShortcutHelp.svelte';
	import { onMount, onDestroy } from 'svelte';
	import {
		startPolling,
		stopPolling,
		isPaused,
		getQueueSlots,
		getSpeedBytesPerSec,
		getSpeedHistory,
		getTotalRemainingBytes,
		refreshQueue
	} from '$lib/stores/queue.svelte';
	import { startHistoryPolling, stopHistoryPolling } from '$lib/stores/history.svelte';
	import { startWarningsPolling, stopWarningsPolling } from '$lib/stores/warnings.svelte';
	import { faviconForState, type AppState } from '$lib/favicon';
	import { handleGlobalShortcut, registerShortcut } from '$lib/shortcuts.svelte';
	import { formatSpeed, formatSize, formatETA } from '$lib/utils';

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

	const eta = $derived.by(() => {
		const speed = getSpeedBytesPerSec();
		const remaining = getTotalRemainingBytes();
		if (speed <= 0 || remaining <= 0) return '--';
		return formatETA(remaining / speed) || '--';
	});
</script>

<svelte:head>
	<title>{titleEmoji} {getQueueSlots().length} item{getQueueSlots().length !== 1 ? 's' : ''} | GoNZBD</title>
	<link rel="icon" type="image/svg+xml" href={faviconHref} />
</svelte:head>

<svelte:window onkeydown={handleGlobalShortcut} />

<div class="flex h-screen w-screen overflow-hidden bg-background text-foreground">
	<!-- Left Side Navigation Rail -->
	<SidebarNav paused={isPaused()} onpausetoggle={refreshQueue} />

	<!-- Main Content Layout -->
	<div class="flex flex-1 flex-col overflow-hidden">
		<ConnectionOverlay />

		<main class="flex-1 overflow-y-auto px-6 py-6 space-y-6">
			<WarningsBanner />

			<!-- Material 3 Quick Stats Grid -->
			<div data-testid="status-bar" class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
				<!-- Speed StatCard -->
				<StatCard
					title="Download Speed"
					value={formatSpeed(getSpeedBytesPerSec())}
					status={appState === 'downloading' ? 'active' : 'idle'}
					sparklineData={getSpeedHistory()}
				>
					<SpeedLimitControl />
				</StatCard>

				<!-- Remaining Bytes StatCard -->
				<StatCard
					title="Remaining Data"
					value={formatSize(getTotalRemainingBytes())}
					status={getTotalRemainingBytes() > 0 && !isPaused() ? 'active' : 'idle'}
				>
					<div class="flex items-center gap-1.5 text-xs text-muted-foreground font-medium">
						{#if isPaused()}
							<span class="text-amber-500 font-bold uppercase tracking-wider">Paused</span>
						{:else}
							<span>ETA:</span>
							<span class="text-foreground font-bold">{eta}</span>
						{/if}
					</div>
				</StatCard>

				<!-- Active Jobs / Slots StatCard -->
				<StatCard
					title="Queue Slots"
					value={`${getQueueSlots().length} Job${getQueueSlots().length !== 1 ? 's' : ''}`}
					status={getQueueSlots().length > 0 ? 'active' : 'idle'}
				/>
			</div>

			<!-- Queue Section -->
			<section>
				<div class="mb-3 flex items-center gap-3">
					<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">Queue</h2>
					<div class="h-px flex-1 bg-border"></div>
				</div>
				<QueueTable />
			</section>

			<!-- History Section -->
			<section>
				<div class="mb-3 flex items-center gap-3">
					<h2 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">History</h2>
					<div class="h-px flex-1 bg-border"></div>
				</div>
				<HistoryTable />
			</section>
		</main>
	</div>
</div>

<Toast />
<ShortcutHelp bind:open={helpOpen} />
