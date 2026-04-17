<script lang="ts">
	import { getApiKey, setApiKey, hasApiKey } from '$lib/stores/apikey.svelte';
	import { fetchVersion } from '$lib/api';
	import { Button } from '$lib/components/ui/button';
	import Navbar from '$lib/components/Navbar.svelte';
	import QueueTable from '$lib/components/QueueTable.svelte';
	import HistoryTable from '$lib/components/HistoryTable.svelte';
	import WarningsPanel from '$lib/components/WarningsPanel.svelte';
	import StatusBar from '$lib/components/StatusBar.svelte';
	import Toast from '$lib/components/Toast.svelte';
	import { Tabs } from 'bits-ui';
	import { Badge } from '$lib/components/ui/badge';
	import { onMount, onDestroy } from 'svelte';
	import { startPolling, stopPolling, isPaused, getSpeedBytesPerSec, formatSpeed } from '$lib/stores/queue.svelte';
	import { startHistoryPolling, stopHistoryPolling } from '$lib/stores/history.svelte';
	import { startWarningsPolling, stopWarningsPolling, getWarningCount } from '$lib/stores/warnings.svelte';

	let keyInput = $state('');
	let connectionStatus = $state<string | null>(null);
	let connecting = $state(false);
	let activeTab = $state('queue');

	async function connect() {
		const key = keyInput.trim();
		if (!key) return;
		connecting = true;
		connectionStatus = null;
		try {
			await fetchVersion(key);
			setApiKey(key);
			startPolling();
			startHistoryPolling();
			startWarningsPolling();
		} catch (e) {
			connectionStatus = `Failed: ${e instanceof Error ? e.message : String(e)}`;
		} finally {
			connecting = false;
		}
	}

	onMount(() => {
		if (hasApiKey()) {
			startPolling();
			startHistoryPolling();
			startWarningsPolling();
		}
	});

	onDestroy(() => {
		stopPolling();
		stopHistoryPolling();
		stopWarningsPolling();
	});
</script>

{#if !hasApiKey()}
	<main class="flex min-h-screen items-center justify-center bg-gray-50">
		<div class="w-full max-w-sm space-y-4 rounded-lg border bg-white p-6 shadow-sm">
			<h1 class="text-2xl font-bold text-gray-900">SABnzbd-Go</h1>
			<p class="text-sm text-gray-600">
				Enter your API key to connect. Find it in your
				<code class="rounded bg-gray-100 px-1 text-xs">sabnzbd.yaml</code> config file.
			</p>
			<input
				type="text"
				bind:value={keyInput}
				placeholder="API key"
				class="w-full rounded-md border px-3 py-2 text-sm focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500"
				onkeydown={(e) => e.key === 'Enter' && connect()}
			/>
			{#if connectionStatus}
				<p class="text-sm text-red-600">{connectionStatus}</p>
			{/if}
			<Button onclick={connect} disabled={connecting || !keyInput.trim()} class="w-full">
				{connecting ? 'Connecting...' : 'Connect'}
			</Button>
		</div>
	</main>
{:else}
	<div class="flex min-h-screen flex-col bg-gray-50">
		<Navbar paused={isPaused()} speed={formatSpeed(getSpeedBytesPerSec())} onpausetoggle={() => {}} />
		<StatusBar />

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
{/if}
