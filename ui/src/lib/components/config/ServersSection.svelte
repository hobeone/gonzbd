<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import type { ServerConfig } from '$lib/types';

	let {
		configData,
		onAddServer,
		onEditServer,
		onTestServer,
		onToggleServer
	}: {
		configData: Record<string, any>;
		onAddServer: () => void;
		onEditServer: (server: ServerConfig) => void;
		onTestServer: (server: ServerConfig) => Promise<{ ok: boolean; message: string }>;
		onToggleServer: (server: ServerConfig, enabled: boolean) => void;
	} = $props();

	let testingServer = $state<string | null>(null);
	let testResults = $state<Record<string, { ok: boolean; message: string }>>({});

	async function handleTest(server: ServerConfig) {
		testingServer = server.name;
		// Clear previous result for this server
		const next = { ...testResults };
		delete next[server.name];
		testResults = next;

		const result = await onTestServer(server);
		testResults = { ...testResults, [server.name]: result };
		testingServer = null;

		// Auto-dismiss after 8 seconds
		const name = server.name;
		setTimeout(() => {
			if (testResults[name]) {
				const updated = { ...testResults };
				delete updated[name];
				testResults = updated;
			}
		}, 8000);
	}

	function dismissResult(name: string) {
		const updated = { ...testResults };
		delete updated[name];
		testResults = updated;
	}

	function allDisabled(): boolean {
		const servers: ServerConfig[] = configData.servers ?? [];
		return servers.length > 0 && servers.every((s) => !s.enable);
	}
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">Usenet Servers</h3>
			<p class="text-sm text-muted-foreground">Manage your NNTP server connections.</p>
		</div>
		<Button size="sm" onclick={onAddServer}>+ Add Server</Button>
	</div>
	<Separator />

	{#if allDisabled()}
		<div class="flex items-center gap-2.5 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 flex-shrink-0 text-amber-500 dark:text-amber-400">
				<path fill-rule="evenodd" d="M6.701 2.25c.577-1 2.02-1 2.598 0l5.196 9a1.5 1.5 0 0 1-1.299 2.25H2.804a1.5 1.5 0 0 1-1.3-2.25l5.197-9ZM8 5a.75.75 0 0 1 .75.75v2a.75.75 0 0 1-1.5 0v-2A.75.75 0 0 1 8 5Zm0 6.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Z" clip-rule="evenodd" />
			</svg>
			<span>All servers are disabled. Downloads will not start until at least one server is enabled.</span>
		</div>
	{/if}

	<div class="space-y-3">
		{#if !configData.servers || configData.servers.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500 dark:text-gray-400">
				No servers configured.
			</div>
		{:else}
			{#each configData.servers as server}
				{@const isTesting = testingServer === server.name}
				{@const result = testResults[server.name]}

				<div class="rounded-lg border {server.enable ? 'border-gray-200 dark:border-gray-700' : 'border-gray-200/60 dark:border-gray-700/60 opacity-60'}">
					<!-- Header row: toggle + name + badges -->
					<div class="flex items-center gap-3 px-4 py-3">
						<label class="flex items-center cursor-pointer flex-shrink-0" title={server.enable ? 'Disable server' : 'Enable server'}>
							<input
								type="checkbox"
								checked={server.enable}
								onchange={() => onToggleServer(server, !server.enable)}
								class="size-4 rounded border-gray-300 dark:border-gray-600 text-blue-600 focus:ring-blue-500 cursor-pointer"
							/>
						</label>
						<div class="min-w-0 flex-1">
							<div class="flex items-center gap-2">
								<span class="font-medium text-sm text-gray-900 dark:text-gray-100 truncate">{server.name}</span>
								{#if server.ssl}
									<span class="inline-flex items-center rounded bg-blue-50 dark:bg-blue-950 px-1.5 py-0 text-[9px] font-bold text-blue-600 dark:text-blue-400 ring-1 ring-inset ring-blue-500/20">TLS</span>
								{/if}
								{#if !server.enable}
									<Badge variant="destructive" class="py-0 h-3.5 text-[9px] uppercase tracking-tighter opacity-70">Disabled</Badge>
								{/if}
							</div>
							<div class="mt-0.5 font-mono text-xs text-gray-500 dark:text-gray-400 truncate">
								{server.host}:{server.port}
							</div>
						</div>
					</div>

					<!-- Details + actions row -->
					<div class="flex flex-wrap items-center gap-x-4 gap-y-1 border-t border-gray-100 dark:border-gray-800 bg-gray-50/50 dark:bg-gray-800/30 px-4 py-2 text-xs text-gray-600 dark:text-gray-400">
						<span>
							<span class="text-gray-400 dark:text-gray-500">User:</span>
							<span class="ml-1 font-medium">{server.username || 'anonymous'}</span>
						</span>
						<span>
							<span class="text-gray-400 dark:text-gray-500">Connections:</span>
							<span class="ml-1 font-bold">{server.connections}</span>
						</span>
						<span>
							<span class="text-gray-400 dark:text-gray-500">Priority:</span>
							<span class="ml-1 font-bold">{server.priority}</span>
						</span>

						<div class="ml-auto flex items-center gap-1">
							<Button variant="ghost" size="xs" onclick={() => handleTest(server)} disabled={isTesting} title="Test connection">
								{#if isTesting}
									<svg class="mr-1 size-3 animate-spin" viewBox="0 0 24 24" fill="none">
										<circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
										<path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
									</svg>
									Testing…
								{:else}
									Test
								{/if}
							</Button>
							<Button variant="ghost" size="xs" onclick={() => onEditServer(server)} title="Edit server">Edit</Button>
						</div>
					</div>

					<!-- Inline test result banner -->
					{#if result}
						<div class="flex items-center gap-2 border-t px-4 py-2 text-xs
							{result.ok
								? 'border-green-200 bg-green-50 text-green-700 dark:border-green-800 dark:bg-green-950 dark:text-green-300'
								: 'border-red-200 bg-red-50 text-red-700 dark:border-red-800 dark:bg-red-950 dark:text-red-300'
							}"
						>
							{#if result.ok}
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 flex-shrink-0">
									<path fill-rule="evenodd" d="M12.416 3.376a.75.75 0 0 1 .208 1.04l-5 7.5a.75.75 0 0 1-1.154.114l-3-3a.75.75 0 0 1 1.06-1.06l2.353 2.353 4.493-6.74a.75.75 0 0 1 1.04-.207Z" clip-rule="evenodd" />
								</svg>
							{:else}
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5 flex-shrink-0">
									<path fill-rule="evenodd" d="M8 15A7 7 0 1 0 8 1a7 7 0 0 0 0 14ZM8 4a.75.75 0 0 1 .75.75v3a.75.75 0 0 1-1.5 0v-3A.75.75 0 0 1 8 4Zm0 8a.908.908 0 1 1 0-1.817.908.908 0 0 1 0 1.817Z" clip-rule="evenodd" />
								</svg>
							{/if}
							<span class="flex-1">{result.message}</span>
							<button
								onclick={() => dismissResult(server.name)}
								class="flex-shrink-0 rounded p-0.5 hover:bg-black/10 dark:hover:bg-white/10"
								aria-label="Dismiss"
							>
								<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3">
									<path d="M5.28 4.22a.75.75 0 0 0-1.06 1.06L6.94 8l-2.72 2.72a.75.75 0 1 0 1.06 1.06L8 9.06l2.72 2.72a.75.75 0 1 0 1.06-1.06L9.06 8l2.72-2.72a.75.75 0 0 0-1.06-1.06L8 6.94 5.28 4.22Z" />
								</svg>
							</button>
						</div>
					{/if}
				</div>
			{/each}
		{/if}
	</div>
</section>
