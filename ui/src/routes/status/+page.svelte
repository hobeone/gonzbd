<script lang="ts">
	import { onMount, onDestroy } from 'svelte';
	import HelpCircle from '@lucide/svelte/icons/help-circle';
	import Navbar from '$lib/components/Navbar.svelte';
	import {
		fetchStatusOverview,
		fetchCheckUpdate,
		fetchBuildInfo,
		fetchRedactedConfig,
		testServerConnection,
		testDiskSpeed,
		type StatusOverviewResponse,
		type CheckUpdateResult,
		type TestConnectionResult,
		type TestDiskSpeedResult,
		type BuildInfoResponse,
		type RedactedConfig,
		type RedactedServerConfig
	} from '$lib/api';
	import { getServerStats } from '$lib/stores/queue.svelte';
	import { startTelemetry, stopTelemetry } from '$lib/stores/telemetry.svelte';

	let overview = $state<StatusOverviewResponse | null>(null);
	let overviewError = $state('');
	let overviewLoading = $state(false);

	let updateCheck = $state<CheckUpdateResult | null>(null);
	let updateCheckLoading = $state(false);

	let buildInfo = $state<BuildInfoResponse | null>(null);
	let buildInfoError = $state('');
	let buildInfoLoading = $state(false);

	let configData = $state<RedactedConfig | null>(null);
	let configError = $state('');
	let configLoading = $state(false);

	function loadBuildInfo() {
		buildInfoLoading = true;
		buildInfoError = '';
		fetchBuildInfo()
			.then((res) => {
				buildInfo = res;
			})
			.catch((e) => {
				buildInfoError = e instanceof Error ? e.message : 'Failed to load build info';
			})
			.finally(() => {
				buildInfoLoading = false;
			});
	}

	function loadConfig() {
		configLoading = true;
		configError = '';
		fetchRedactedConfig()
			.then((res) => {
				configData = res.config;
			})
			.catch((e) => {
				configError = e instanceof Error ? e.message : 'Failed to load config';
			})
			.finally(() => {
				configLoading = false;
			});
	}

	function loadOverview() {
		overviewLoading = true;
		overviewError = '';
		fetchStatusOverview()
			.then((res) => {
				overview = res;
			})
			.catch((e) => {
				overviewError = e instanceof Error ? e.message : 'Failed to load status';
			})
			.finally(() => {
				overviewLoading = false;
			});
	}

	function loadUpdateCheck() {
		updateCheckLoading = true;
		fetchCheckUpdate()
			.then((res) => {
				updateCheck = res.result;
			})
			.catch(() => {
				updateCheck = { status: 'unknown' };
			})
			.finally(() => {
				updateCheckLoading = false;
			});
	}

	function refresh() {
		loadOverview();
		loadUpdateCheck();
		loadBuildInfo();
		loadConfig();
	}

	let servers = $derived(getServerStats());
	let serverConfigByName = $derived.by((): Record<string, RedactedServerConfig> => {
		const map: Record<string, RedactedServerConfig> = {};
		for (const sc of configData?.servers ?? []) {
			map[sc.name] = sc;
		}
		return map;
	});

	const sslVerifyLabels: Record<number, string> = {
		0: 'None',
		1: 'Minimal',
		2: 'Hostname',
		3: 'Strict'
	};
	let testingServer = $state<string | null>(null);
	let connectionResults = $state<Record<string, TestConnectionResult>>({});

	let diskSpeedTesting = $state(false);
	let diskSpeedResult = $state<TestDiskSpeedResult | null>(null);

	// This page is the only route that renders server data outside the
	// main dashboard, so it must start its own WebSocket subscription
	// (reference-counted — see websocket.svelte.ts) rather than assuming one
	// is already running. Symmetric stop on unmount so a repeat visit to
	// /status doesn't leak a duplicate handler.
	onMount(() => {
		startTelemetry();
	});
	onDestroy(() => {
		stopTelemetry();
	});

	async function runConnectionTest(name: string) {
		testingServer = name;
		try {
			const res = await testServerConnection(name);
			connectionResults = { ...connectionResults, [name]: res.result };
		} catch (e) {
			connectionResults = {
				...connectionResults,
				[name]: { ok: false, error: e instanceof Error ? e.message : 'Test failed' }
			};
		} finally {
			testingServer = null;
		}
	}

	async function runDiskSpeedTest() {
		diskSpeedTesting = true;
		try {
			const res = await testDiskSpeed();
			diskSpeedResult = res.result;
		} catch (e) {
			diskSpeedResult = { ok: false, error: e instanceof Error ? e.message : 'Test failed' };
		} finally {
			diskSpeedTesting = false;
		}
	}

	onMount(refresh);

	function formatUptime(seconds: number): string {
		const days = Math.floor(seconds / 86400);
		const hours = Math.floor((seconds % 86400) / 3600);
		const mins = Math.floor((seconds % 3600) / 60);
		return `${days}d ${hours}h ${mins}m`;
	}

	function formatBytes(bytes: number): string {
		if (bytes <= 0) return '0 B';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
		return `${(bytes / Math.pow(1024, i)).toFixed(1)} ${units[i]}`;
	}

	function formatBuildDate(iso: string): string {
		if (!iso) return 'unknown';
		const d = new Date(iso);
		return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
	}
</script>

<svelte:head><title>Status - GoNZBD</title></svelte:head>

<Navbar />

<div class="mx-auto max-w-4xl p-6">
	<h1 class="mb-4 text-2xl font-semibold text-m3-on-surface">Status</h1>

	<button
		class="mb-4 rounded-full bg-m3-primary px-4 py-2 text-sm text-m3-on-primary"
		onclick={refresh}
		disabled={overviewLoading || updateCheckLoading || buildInfoLoading || configLoading}
	>
		{overviewLoading || updateCheckLoading || buildInfoLoading || configLoading
			? 'Refreshing...'
			: 'Refresh'}
	</button>

	{#if overviewError}
		<p class="text-red-500">{overviewError}</p>
	{:else if overview}
		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">General Info</h2>
			<dl class="grid grid-cols-[180px_1fr] gap-x-4 gap-y-3 text-sm">
				<dt class="text-m3-on-surface/60">Version</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.version} ({overview.general.commit})</dd>
				<dt class="text-m3-on-surface/60">Uptime</dt>
				<dd class="text-m3-on-surface">{formatUptime(overview.general.uptime_seconds)}</dd>
				<dt class="text-m3-on-surface/60">Go version</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.go_version}</dd>
				<dt class="text-m3-on-surface/60">Hostname</dt>
				<dd class="text-m3-on-surface">{overview.general.hostname}</dd>
				<dt class="text-m3-on-surface/60">Local IP</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.local_ip}</dd>
				<dt class="text-m3-on-surface/60">Config path</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.config_path}</dd>
				<dt class="text-m3-on-surface/60">par2</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.par2.path || 'not found'} {overview.general.par2.version}</dd>
				<dt class="text-m3-on-surface/60">unrar</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.unrar.path || 'not found'} {overview.general.unrar.version}</dd>
				<dt class="text-m3-on-surface/60">7-Zip</dt>
				<dd class="font-mono text-m3-on-surface">{overview.general.sevenzip.path || 'not found'} {overview.general.sevenzip.version}</dd>
				<dt class="flex items-center gap-1 text-m3-on-surface/60">
					Update
					<HelpCircle
						class="size-3.5 cursor-help text-muted-foreground/60"
						title="Compares this build's version against the latest published GitHub Release for hobeone/gonzbd. Requires a version baked in at build time (git describe or -ldflags -X main.Version=...) and outbound network access to api.github.com; otherwise this shows 'unknown' with an explanation."
					/>
				</dt>
				<dd>
					{#if updateCheckLoading}
						<span class="inline-flex items-center gap-1.5 text-xs text-m3-on-surface/60">
							<svg class="animate-spin h-3.5 w-3.5 text-m3-primary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
							checking…
						</span>
					{:else if updateCheck?.status === 'update_available'}
						<span class="inline-flex items-center gap-1.5 text-xs font-semibold text-amber-600 bg-amber-50 px-2 py-0.5 rounded-full border border-amber-200">
							<span class="relative flex h-2 w-2">
								<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-amber-400 opacity-75"></span>
								<span class="relative inline-flex rounded-full h-2 w-2 bg-amber-500"></span>
							</span>
							update available: {updateCheck.latest_version}
						</span>
					{:else if updateCheck?.status === 'up_to_date'}
						<span class="inline-flex items-center gap-1.5 text-xs text-green-600 bg-green-50 px-2 py-0.5 rounded-full border border-green-200">
							<span class="h-2 w-2 rounded-full bg-green-500"></span>
							up to date
						</span>
					{:else}
						<span class="text-m3-on-surface/60">unknown</span>
						{#if updateCheck?.reason}
							<span class="block text-xs text-m3-on-surface/40">{updateCheck.reason}</span>
						{/if}
					{/if}
				</dd>
			</dl>
		</section>

		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">System Info</h2>
			<dl class="grid grid-cols-[180px_1fr] gap-x-4 gap-y-3 text-sm">
				<dt class="text-m3-on-surface/60">OS / Arch</dt>
				<dd class="text-m3-on-surface">{overview.system.os} / {overview.system.arch}</dd>
				<dt class="text-m3-on-surface/60">Article cache usage</dt>
				<dd class="text-m3-on-surface">{formatBytes(overview.system.article_cache_bytes)}</dd>
				<dt class="text-m3-on-surface/60">Download dir free space</dt>
				<dd class="text-m3-on-surface">
					{formatBytes(overview.system.download_dir_free_bytes)}
					<span class="text-m3-on-surface/50"
						>(min: {formatBytes(overview.system.min_free_space_bytes)})</span
					>
				</dd>
				<dt class="text-m3-on-surface/60">Disk speed</dt>
				<dd class="flex items-center gap-2">
					<button
						class="inline-flex items-center gap-1.5 rounded-full bg-m3-secondary px-3 py-1 text-xs text-m3-on-secondary disabled:opacity-50"
						onclick={runDiskSpeedTest}
						disabled={diskSpeedTesting}
					>
						{#if diskSpeedTesting}
							<svg class="animate-spin h-3 w-3 text-m3-on-secondary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
							Testing...
						{:else}
							Test Disk Speed
						{/if}
					</button>
					{#if diskSpeedResult}
						{#if diskSpeedResult.ok}
							<span class="inline-flex items-center gap-1 text-xs font-semibold text-green-600 bg-green-50 px-2 py-0.5 rounded-full border border-green-200">
								{diskSpeedResult.mb_per_sec?.toFixed(1)} MB/s
							</span>
						{:else}
							<span class="inline-flex items-center gap-1 text-xs text-red-600 bg-red-50 px-2 py-0.5 rounded-full border border-red-200">
								Failed: {diskSpeedResult.error}
							</span>
						{/if}
					{/if}
				</dd>
			</dl>
		</section>

		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">Build Info</h2>
			{#if buildInfoError}
				<p class="text-red-500">{buildInfoError}</p>
			{:else if buildInfoLoading && !buildInfo}
				<p class="text-sm text-m3-on-surface/60">Loading...</p>
			{:else if buildInfo}
				<dl class="mb-4 grid grid-cols-[180px_1fr] gap-x-4 gap-y-3 text-sm">
					<dt class="text-m3-on-surface/60">Version</dt>
					<dd class="font-mono text-m3-on-surface">{buildInfo.version} ({buildInfo.commit})</dd>
					<dt class="text-m3-on-surface/60">Build date</dt>
					<dd class="text-m3-on-surface">{formatBuildDate(buildInfo.build_date)}</dd>
					<dt class="text-m3-on-surface/60">Go version</dt>
					<dd class="font-mono text-m3-on-surface">{buildInfo.go_version}</dd>
				</dl>
				<h3 class="mb-2 text-sm font-medium text-m3-on-surface/80">
					Dependencies ({buildInfo.deps.length})
				</h3>
				<div class="max-h-72 overflow-y-auto rounded-2xl border border-m3-outline/10">
					<table class="w-full text-xs">
						<tbody>
							{#each buildInfo.deps as dep (dep.path)}
								<tr class="border-b border-m3-outline/10 last:border-0">
									<td class="px-3 py-1.5 font-mono text-m3-on-surface">{dep.path}</td>
									<td class="px-3 py-1.5 font-mono text-m3-on-surface/60">{dep.version}</td>
								</tr>
							{/each}
						</tbody>
					</table>
				</div>
			{/if}
		</section>

		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">News Servers</h2>
			{#each servers as server (server.name)}
				<div class="mb-3 rounded-2xl border border-m3-outline/10 p-4 transition-all hover:bg-m3-surface-variant/5">
					<div class="flex items-center justify-between">
						<span class="font-medium text-m3-on-surface">{server.name} ({server.host}:{server.port})</span>
						<span class="text-sm text-m3-on-surface/60">
							{server.active_conns}/{server.max_connections} connections in use
						</span>
					</div>
					{#if serverConfigByName[server.name]}
						{@const sc = serverConfigByName[server.name]}
						<dl class="mt-3 grid grid-cols-[110px_1fr] gap-x-4 gap-y-1 text-xs sm:grid-cols-[110px_1fr_110px_1fr]">
							<dt class="text-m3-on-surface/50">Enabled</dt>
							<dd class="text-m3-on-surface">{sc.enable ? 'Yes' : 'No'}</dd>
							<dt class="text-m3-on-surface/50">SSL</dt>
							<dd class="text-m3-on-surface">
								{sc.ssl ? `Yes (${sslVerifyLabels[sc.ssl_verify] ?? sc.ssl_verify})` : 'No'}
							</dd>
							<dt class="text-m3-on-surface/50">SSL ciphers</dt>
							<dd class="font-mono text-m3-on-surface">{sc.ssl_ciphers || 'default'}</dd>
							<dt class="text-m3-on-surface/50">Priority</dt>
							<dd class="text-m3-on-surface">{sc.priority}</dd>
							<dt class="text-m3-on-surface/50">Required</dt>
							<dd class="text-m3-on-surface">{sc.required ? 'Yes' : 'No'}</dd>
							<dt class="text-m3-on-surface/50">Optional</dt>
							<dd class="text-m3-on-surface">{sc.optional ? 'Yes' : 'No'}</dd>
							<dt class="text-m3-on-surface/50">Retention</dt>
							<dd class="text-m3-on-surface">{sc.retention > 0 ? `${sc.retention} days` : 'unlimited'}</dd>
							<dt class="text-m3-on-surface/50">Timeout</dt>
							<dd class="text-m3-on-surface">{sc.timeout}s</dd>
							<dt class="text-m3-on-surface/50">Pipelining</dt>
							<dd class="text-m3-on-surface">{sc.pipelining_requests} requests</dd>
						</dl>
					{/if}
					<div class="mt-3 flex items-center gap-3">
						<button
							class="inline-flex items-center gap-1.5 rounded-full bg-m3-secondary px-3 py-1 text-xs text-m3-on-secondary disabled:opacity-50"
							onclick={() => runConnectionTest(server.name)}
							disabled={testingServer === server.name}
						>
							{#if testingServer === server.name}
								<svg class="animate-spin h-3 w-3 text-m3-on-secondary" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
								Testing...
							{:else}
								Test Connection
							{/if}
						</button>
						{#if connectionResults[server.name]}
							{@const result = connectionResults[server.name]}
							{#if result.ok}
								<span class="inline-flex items-center gap-1.5 text-xs text-green-600 bg-green-50 px-2.5 py-0.5 rounded-full border border-green-200">
									<span class="relative flex h-1.5 w-1.5">
										<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-green-400 opacity-75"></span>
										<span class="relative inline-flex rounded-full h-1.5 w-1.5 bg-green-500"></span>
									</span>
									Connected ({result.latency_ms}ms)
								</span>
							{:else if result.likely_connection_limit}
								<span class="inline-flex items-center gap-1.5 text-xs text-amber-600 bg-amber-50 px-2.5 py-0.5 rounded-full border border-amber-200">
									<span class="h-1.5 w-1.5 rounded-full bg-amber-500"></span>
									Connection limit reached ({result.error})
								</span>
							{:else}
								<span class="inline-flex items-center gap-1.5 text-xs text-red-600 bg-red-50 px-2.5 py-0.5 rounded-full border border-red-200">
									<span class="relative flex h-1.5 w-1.5">
										<span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-red-400 opacity-75"></span>
										<span class="relative inline-flex rounded-full h-1.5 w-1.5 bg-red-500"></span>
									</span>
									Failed: {result.error}
								</span>
							{/if}
						{/if}
					</div>
				</div>
			{/each}
		</section>

		<section class="mb-6 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-sm transition-all duration-300 hover:shadow-md hover:border-m3-primary/30">
			<h2 class="mb-4 text-lg font-medium text-m3-on-surface">Config</h2>
			<p class="mb-3 text-xs text-m3-on-surface/50">
				Passwords, API keys, and other secrets are redacted.
			</p>
			{#if configError}
				<p class="text-red-500">{configError}</p>
			{:else if configLoading && !configData}
				<p class="text-sm text-m3-on-surface/60">Loading...</p>
			{:else if configData}
				<pre class="max-h-96 overflow-auto rounded-2xl border border-m3-outline/10 bg-m3-surface-variant/10 p-4 text-xs text-m3-on-surface">{JSON.stringify(configData, null, 2)}</pre>
			{/if}
		</section>
	{/if}
</div>
