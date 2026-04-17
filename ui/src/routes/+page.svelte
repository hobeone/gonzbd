<script lang="ts">
	import { getApiKey, setApiKey, hasApiKey } from '$lib/stores/apikey.svelte';
	import { fetchVersion } from '$lib/api';
	import { Button } from '$lib/components/ui/button';

	let keyInput = $state('');
	let connectionStatus = $state<string | null>(null);
	let connecting = $state(false);

	async function connect() {
		const key = keyInput.trim();
		if (!key) return;

		connecting = true;
		connectionStatus = null;
		try {
			const res = await fetchVersion(key);
			setApiKey(key);
			connectionStatus = `Connected — v${res.version}`;
		} catch (e) {
			connectionStatus = `Connection failed: ${e instanceof Error ? e.message : String(e)}`;
		} finally {
			connecting = false;
		}
	}
</script>

{#if hasApiKey()}
	<main class="flex min-h-screen items-center justify-center bg-gray-50">
		<div class="text-center">
			<h1 class="text-4xl font-bold text-gray-900">SABnzbd-Go</h1>
			<p class="mt-2 text-gray-600">Connected</p>
		</div>
	</main>
{:else}
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
				<p class="text-sm {connectionStatus.startsWith('Connected') ? 'text-green-600' : 'text-red-600'}">
					{connectionStatus}
				</p>
			{/if}
			<Button onclick={connect} disabled={connecting || !keyInput.trim()} class="w-full">
				{connecting ? 'Connecting...' : 'Connect'}
			</Button>
		</div>
	</main>
{/if}
