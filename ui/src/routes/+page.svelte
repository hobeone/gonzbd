<script lang="ts">
	import Navbar from '$lib/components/Navbar.svelte';
	import QueueTable from '$lib/components/QueueTable.svelte';
	import HistoryTable from '$lib/components/HistoryTable.svelte';
	import WarningsPanel from '$lib/components/WarningsPanel.svelte';
	import StatusBar from '$lib/components/StatusBar.svelte';
	import Toast from '$lib/components/Toast.svelte';
	import { Tabs } from 'bits-ui';
	import { Badge } from '$lib/components/ui/badge';
	import { onMount, onDestroy } from 'svelte';
	import { startPolling, stopPolling, isPaused, getSpeedBytesPerSec, formatSpeed, getQueueSlots, getError } from '$lib/stores/queue.svelte';
	import { startHistoryPolling, stopHistoryPolling } from '$lib/stores/history.svelte';
	import { startWarningsPolling, stopWarningsPolling, getWarningCount } from '$lib/stores/warnings.svelte';

	let activeTab = $state('queue');

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

	<div class="mx-auto w-full max-w-7xl flex-1 px-4 pt-4">
		<Tabs.Root bind:value={activeTab}>
			<Tabs.List class="mb-4 flex gap-1 border-b">
				<Tabs.Trigger
					value="queue"
					class="border-b-2 px-4 py-2 text-sm font-medium transition-colors data-[state=active]:border-blue-600 data-[state=active]:text-blue-600 data-[state=inactive]:border-transparent data-[state=inactive]:text-gray-500 data-[state=inactive]:hover:text-gray-700"
				>
					Queue
				</Tabs.Trigger>
				<Tabs.Trigger
					value="history"
					class="border-b-2 px-4 py-2 text-sm font-medium transition-colors data-[state=active]:border-blue-600 data-[state=active]:text-blue-600 data-[state=inactive]:border-transparent data-[state=inactive]:text-gray-500 data-[state=inactive]:hover:text-gray-700"
				>
					History
				</Tabs.Trigger>
				<Tabs.Trigger
					value="warnings"
					class="relative border-b-2 px-4 py-2 text-sm font-medium transition-colors data-[state=active]:border-blue-600 data-[state=active]:text-blue-600 data-[state=inactive]:border-transparent data-[state=inactive]:text-gray-500 data-[state=inactive]:hover:text-gray-700"
				>
					Warnings
					{#if getWarningCount() > 0}
						<Badge variant="destructive" class="ml-1.5 px-1.5 py-0 text-xs">
							{getWarningCount()}
						</Badge>
					{/if}
				</Tabs.Trigger>
			</Tabs.List>

			<Tabs.Content value="queue">
				<QueueTable />
			</Tabs.Content>

			<Tabs.Content value="history">
				<HistoryTable />
			</Tabs.Content>

			<Tabs.Content value="warnings">
				<WarningsPanel />
			</Tabs.Content>
		</Tabs.Root>
	</div>
</div>

<Toast />
