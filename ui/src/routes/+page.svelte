<script lang="ts">
	import Navbar from '$lib/components/Navbar.svelte';
	import QueueTable from '$lib/components/QueueTable.svelte';
	import HistoryTable from '$lib/components/HistoryTable.svelte';
	import WarningsBanner from '$lib/components/WarningsBanner.svelte';
	import StatusBar from '$lib/components/StatusBar.svelte';
	import Toast from '$lib/components/Toast.svelte';
	import { onMount, onDestroy } from 'svelte';
	import { startPolling, stopPolling, isPaused, getSpeedBytesPerSec, formatSpeed, getQueueSlots, getError } from '$lib/stores/queue.svelte';
	import { startHistoryPolling, stopHistoryPolling } from '$lib/stores/history.svelte';
	import { startWarningsPolling, stopWarningsPolling } from '$lib/stores/warnings.svelte';

	onMount(() => {
		startPolling();
		startHistoryPolling();
		startWarningsPolling();
	});

	onDestroy(() => {
		stopPolling();
		stopHistoryPolling();
		stopWarningsPolling();
	});
</script>

<svelte:head>
	<title>{isPaused() ? '⏸' : '▶'} {getQueueSlots().length} item{getQueueSlots().length !== 1 ? 's' : ''} | SABnzbd-Go</title>
</svelte:head>

<div class="flex min-h-screen flex-col bg-gray-50 dark:bg-gray-950">
	<Navbar paused={isPaused()} speed={formatSpeed(getSpeedBytesPerSec())} onpausetoggle={() => {}} />
	<StatusBar />

	{#if getError()}
		<div class="mx-auto w-full max-w-7xl px-4 pt-2">
			<div class="rounded-lg border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700 dark:border-red-800 dark:bg-red-950 dark:text-red-300">
				API unreachable: {getError()}
			</div>
		</div>
	{/if}

	<div class="mx-auto w-full max-w-7xl flex-1 space-y-6 px-4 pt-4 pb-8">
		<WarningsBanner />

		<section>
			<div class="mb-3 flex items-center gap-3">
				<h2 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">Queue</h2>
				<div class="h-px flex-1 bg-gray-200 dark:bg-gray-700"></div>
			</div>
			<QueueTable />
		</section>

		<section>
			<div class="mb-3 flex items-center gap-3">
				<h2 class="text-sm font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">History</h2>
				<div class="h-px flex-1 bg-gray-200 dark:bg-gray-700"></div>
			</div>
			<HistoryTable />
		</section>
	</div>
</div>

<Toast />
