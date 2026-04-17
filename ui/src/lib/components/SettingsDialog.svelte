<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { getApiKey } from '$lib/stores/apikey.svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let config = $state<Record<string, unknown> | null>(null);
	let loading = $state(false);
	let error = $state<string | null>(null);

	async function loadConfig() {
		loading = true;
		error = null;
		try {
			const url = `/api?mode=get_config&apikey=${encodeURIComponent(getApiKey())}&output=json`;
			const res = await fetch(url);
			if (!res.ok) throw new Error(`${res.status}: ${res.statusText}`);
			const data = await res.json();
			config = data.config ?? data;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}

	function renderValue(val: unknown): string {
		if (val === null || val === undefined) return '--';
		if (typeof val === 'string') return val || '(empty)';
		if (typeof val === 'boolean') return val ? 'Yes' : 'No';
		if (typeof val === 'number') return String(val);
		return JSON.stringify(val, null, 2);
	}

	function isSection(val: unknown): val is Record<string, unknown> {
		return typeof val === 'object' && val !== null && !Array.isArray(val);
	}
</script>

<Dialog.Root bind:open onOpenChange={(o) => { if (o) loadConfig(); }}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-50 w-full max-w-2xl max-h-[80vh] -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-lg border bg-white p-6 shadow-lg">
			<Dialog.Title class="text-lg font-semibold">Settings</Dialog.Title>
			<Dialog.Description class="mt-1 text-sm text-gray-500">
				Current configuration (read-only)
			</Dialog.Description>

			<div class="mt-4">
				{#if loading}
					<p class="text-sm text-gray-500">Loading configuration...</p>
				{:else if error}
					<p class="text-sm text-red-600">{error}</p>
				{:else if config}
					{#each Object.entries(config) as [key, val]}
						{#if isSection(val)}
							<details class="mb-2 rounded border">
								<summary class="cursor-pointer bg-gray-50 px-4 py-2 text-sm font-medium text-gray-700">
									{key}
								</summary>
								<div class="divide-y">
									{#each Object.entries(val) as [k, v]}
										<div class="flex gap-4 px-4 py-2 text-sm">
											<span class="w-48 shrink-0 font-mono text-gray-500">{k}</span>
											{#if isSection(v) || Array.isArray(v)}
												<pre class="flex-1 overflow-x-auto whitespace-pre-wrap text-xs text-gray-700">{JSON.stringify(v, null, 2)}</pre>
											{:else}
												<span class="flex-1 text-gray-900">{renderValue(v)}</span>
											{/if}
										</div>
									{/each}
								</div>
							</details>
						{:else}
							<div class="flex gap-4 border-b px-4 py-2 text-sm">
								<span class="w-48 shrink-0 font-mono text-gray-500">{key}</span>
								<span class="flex-1 text-gray-900">{renderValue(val)}</span>
							</div>
						{/if}
					{/each}
				{/if}
			</div>

			<div class="mt-4 flex justify-end">
				<Button variant="outline" onclick={() => (open = false)}>Close</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
