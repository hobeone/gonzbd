<script lang="ts">
	import { fetchServerStats } from '$lib/api';
	import type { ServerSnapshot, ConnSnapshot } from '$lib/types';
	import { onDestroy } from 'svelte';

	let {
		open = $bindable(false)
	}: {
		open?: boolean;
	} = $props();

	let servers = $state<ServerSnapshot[]>([]);
	let loading = $state(false);
	let error = $state('');
	let expandedServers = $state<Set<string>>(new Set());
	let intervalId: ReturnType<typeof setInterval> | undefined;

	function startPolling() {
		loadData();
		intervalId = setInterval(loadData, 2000);
	}

	function stopPolling() {
		if (intervalId) {
			clearInterval(intervalId);
			intervalId = undefined;
		}
	}

	async function loadData() {
		try {
			const resp = await fetchServerStats();
			servers = resp.servers;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to fetch';
		}
	}

	function toggleExpanded(name: string) {
		const next = new Set(expandedServers);
		if (next.has(name)) {
			next.delete(name);
		} else {
			next.add(name);
		}
		expandedServers = next;
	}

	function formatBytes(bytes: number): string {
		if (bytes === 0) return '0 B';
		const units = ['B', 'KB', 'MB', 'GB', 'TB'];
		const i = Math.floor(Math.log(bytes) / Math.log(1024));
		return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
	}

	function formatBps(bps: number): string {
		if (bps === 0) return '0 B/s';
		const units = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
		const i = Math.floor(Math.log(bps) / Math.log(1024));
		return `${(bps / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
	}

	function formatPenalty(unixTs: number): string {
		if (!unixTs) return '';
		const remaining = Math.max(0, unixTs - Math.floor(Date.now() / 1000));
		if (remaining === 0) return '';
		const mins = Math.floor(remaining / 60);
		const secs = remaining % 60;
		return mins > 0 ? `${mins}m ${secs}s` : `${secs}s`;
	}

	function connStatusLabel(c: ConnSnapshot): string {
		if (c.article_id) return c.subject || c.article_id;
		if (c.connected) return 'Idle';
		return 'Disconnected';
	}

	function connDuration(c: ConnSnapshot): string {
		if (!c.since_unix) return '';
		const secs = Math.max(0, Math.floor(Date.now() / 1000) - c.since_unix);
		return `${secs}s`;
	}

	function totalBps(): number {
		return servers.reduce((sum, s) => sum + s.bps, 0);
	}

	function totalActiveConns(): number {
		return servers.reduce((sum, s) => sum + s.active_conns, 0);
	}

	function totalMaxConns(): number {
		return servers.reduce((sum, s) => sum + s.max_connections, 0);
	}

	$effect(() => {
		if (open) {
			startPolling();
		} else {
			stopPolling();
		}
	});

	onDestroy(() => stopPolling());
</script>

{#if open}
	<!-- Backdrop -->
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="fixed inset-0 z-40 bg-black/30 backdrop-blur-sm transition-opacity"
		onclick={() => (open = false)}
	></div>

	<!-- Drawer -->
	<div
		class="fixed top-0 right-0 z-50 flex h-full w-full max-w-lg flex-col border-l border-gray-200 bg-white shadow-2xl dark:border-gray-700 dark:bg-gray-900"
	>
		<!-- Header -->
		<div class="flex items-center justify-between border-b border-gray-200 px-5 py-4 dark:border-gray-700">
			<div class="flex items-center gap-3">
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5 text-indigo-500">
					<path d="M1 4.75C1 3.784 1.784 3 2.75 3h14.5c.966 0 1.75.784 1.75 1.75v10.515a1.75 1.75 0 0 1-1.75 1.75h-1.5c-.078 0-.155-.005-.23-.015H4.48c-.075.01-.152.015-.23.015h-1.5A1.75 1.75 0 0 1 1 15.265V4.75Z" />
				</svg>
				<h2 class="text-base font-semibold text-gray-900 dark:text-gray-100">Server Status</h2>
			</div>
			<button
				onclick={() => (open = false)}
				aria-label="Close"
				class="rounded-md p-1.5 text-gray-400 hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-gray-800 dark:hover:text-gray-300"
			>
				<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5">
					<path d="M6.28 5.22a.75.75 0 0 0-1.06 1.06L8.94 10l-3.72 3.72a.75.75 0 1 0 1.06 1.06L10 11.06l3.72 3.72a.75.75 0 1 0 1.06-1.06L11.06 10l3.72-3.72a.75.75 0 0 0-1.06-1.06L10 8.94 6.28 5.22Z" />
				</svg>
			</button>
		</div>

		<!-- Aggregate stats bar -->
		<div class="grid grid-cols-3 gap-3 border-b border-gray-200 bg-gray-50 px-5 py-3 dark:border-gray-700 dark:bg-gray-800/50">
			<div class="text-center">
				<div class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">Speed</div>
				<div class="mt-0.5 text-lg font-bold text-gray-900 dark:text-gray-100">{formatBps(totalBps())}</div>
			</div>
			<div class="text-center">
				<div class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">Active</div>
				<div class="mt-0.5 text-lg font-bold text-gray-900 dark:text-gray-100">{totalActiveConns()} / {totalMaxConns()}</div>
			</div>
			<div class="text-center">
				<div class="text-xs font-medium uppercase tracking-wider text-gray-500 dark:text-gray-400">Servers</div>
				<div class="mt-0.5 text-lg font-bold text-gray-900 dark:text-gray-100">{servers.length}</div>
			</div>
		</div>

		<!-- Server list -->
		<div class="flex-1 overflow-y-auto p-4 space-y-3">
			{#if error}
				<div class="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-950 dark:text-red-300">
					{error}
				</div>
			{/if}

			{#each servers as server (server.name)}
				{@const penalty = formatPenalty(server.penalty_until)}
				{@const isExpanded = expandedServers.has(server.name)}
				{@const aggrBps = totalBps()}
				{@const speedPct = aggrBps > 0 ? (server.bps / aggrBps) * 100 : 0}

				<div class="overflow-hidden rounded-lg border border-gray-200 dark:border-gray-700">
					<!-- Server header -->
					<button
						onclick={() => toggleExpanded(server.name)}
						class="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-gray-50 dark:hover:bg-gray-800/50"
					>
						<!-- Status dot -->
						<div class="flex-shrink-0">
							{#if !server.enabled}
								<div class="size-2.5 rounded-full bg-gray-400" title="Disabled"></div>
							{:else if penalty}
								<div class="size-2.5 rounded-full bg-amber-500 animate-pulse" title="Penalized"></div>
							{:else if server.active}
								<div class="size-2.5 rounded-full bg-emerald-500" title="Active"></div>
							{:else}
								<div class="size-2.5 rounded-full bg-red-500" title="Inactive"></div>
							{/if}
						</div>

						<!-- Server info -->
						<div class="min-w-0 flex-1">
							<div class="flex items-center gap-2">
								<span class="truncate font-medium text-gray-900 dark:text-gray-100">{server.name}</span>
								{#if server.ssl}
									<span class="rounded bg-green-100 px-1.5 py-0.5 text-[10px] font-bold uppercase text-green-700 dark:bg-green-900 dark:text-green-300">SSL</span>
								{/if}
								{#if server.optional}
									<span class="rounded bg-blue-100 px-1.5 py-0.5 text-[10px] font-bold uppercase text-blue-700 dark:bg-blue-900 dark:text-blue-300">Backup</span>
								{/if}
								<span class="text-xs text-gray-500 dark:text-gray-400">P{server.priority}</span>
							</div>
							<div class="mt-0.5 text-xs text-gray-500 dark:text-gray-400">
								{server.host}:{server.port}
							</div>
						</div>

						<!-- Speed + conn count -->
						<div class="flex-shrink-0 text-right">
							<div class="text-sm font-semibold text-gray-900 dark:text-gray-100">{formatBps(server.bps)}</div>
							<div class="text-xs text-gray-500 dark:text-gray-400">
								{server.active_conns}/{server.max_connections} conns
							</div>
						</div>

						<!-- Expand arrow -->
						<svg
							xmlns="http://www.w3.org/2000/svg"
							viewBox="0 0 20 20"
							fill="currentColor"
							class="size-4 flex-shrink-0 text-gray-400 transition-transform {isExpanded ? 'rotate-180' : ''}"
						>
							<path fill-rule="evenodd" d="M5.22 8.22a.75.75 0 0 1 1.06 0L10 11.94l3.72-3.72a.75.75 0 1 1 1.06 1.06l-4.25 4.25a.75.75 0 0 1-1.06 0L5.22 9.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
						</svg>
					</button>

					<!-- Speed proportion bar -->
					{#if server.enabled && aggrBps > 0}
						<div class="h-1 bg-gray-100 dark:bg-gray-800">
							<div
								class="h-full bg-indigo-500 transition-all duration-300"
								style="width: {Math.max(speedPct, 1)}%"
							></div>
						</div>
					{/if}

					<!-- Expanded details -->
					{#if isExpanded}
						<div class="border-t border-gray-200 bg-gray-50 dark:border-gray-700 dark:bg-gray-800/30">
							<!-- Stats -->
							<div class="grid grid-cols-2 gap-x-4 gap-y-1 px-4 py-3 text-xs">
								<div>
									<span class="text-gray-500 dark:text-gray-400">Total Downloaded:</span>
									<span class="ml-1 font-medium text-gray-900 dark:text-gray-100">{formatBytes(server.total_bytes)}</span>
								</div>
								<div>
									<span class="text-gray-500 dark:text-gray-400">Pipelining:</span>
									<span class="ml-1 font-medium text-gray-900 dark:text-gray-100">{server.pipelining || 1}</span>
								</div>
							</div>

							{#if penalty}
								<div class="mx-4 mb-3 flex items-center gap-2 rounded-md bg-amber-50 px-3 py-2 text-xs dark:bg-amber-950">
									<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 text-amber-600 dark:text-amber-400">
										<path fill-rule="evenodd" d="M6.701 2.25c.577-1 2.02-1 2.598 0l5.196 9a1.5 1.5 0 0 1-1.299 2.25H2.804a1.5 1.5 0 0 1-1.3-2.25l5.197-9ZM8 5a.75.75 0 0 1 .75.75v2a.75.75 0 0 1-1.5 0v-2A.75.75 0 0 1 8 5Zm0 6.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Z" clip-rule="evenodd" />
									</svg>
									<span class="font-medium text-amber-700 dark:text-amber-300">Penalized — {penalty} remaining</span>
								</div>
							{/if}

							{#if !server.enabled}
								<div class="mx-4 mb-3 flex items-center gap-2 rounded-md bg-gray-100 px-3 py-2 text-xs dark:bg-gray-800">
									<span class="font-medium text-gray-600 dark:text-gray-400">Server disabled in configuration</span>
								</div>
							{/if}

							<!-- Per-connection table -->
							<div class="px-4 pb-3">
								<div class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-gray-400 mb-2">
									Connections
								</div>
								<div class="space-y-1">
									{#each server.connections.toSorted((a, b) => a.index - b.index) as conn (conn.index)}
										<div class="flex items-center gap-2 rounded-md px-2 py-1.5 text-xs {conn.article_id ? 'bg-white dark:bg-gray-900' : 'bg-transparent'}">
											<span class="w-6 flex-shrink-0 font-mono text-gray-400 dark:text-gray-500">#{conn.index}</span>
											{#if conn.article_id}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-emerald-500 animate-pulse"></div>
												<span class="min-w-0 flex-1 truncate text-gray-700 dark:text-gray-300" title={conn.subject || conn.article_id}>
													{conn.subject || conn.article_id}
												</span>
												<span class="flex-shrink-0 text-gray-400 dark:text-gray-500">{formatBytes(conn.bytes)}</span>
												<span class="flex-shrink-0 font-mono text-gray-400 dark:text-gray-500">{connDuration(conn)}</span>
											{:else if conn.connected}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-blue-400 dark:bg-blue-500"></div>
												<span class="text-gray-400 dark:text-gray-500">Idle</span>
											{:else}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-gray-300 dark:bg-gray-600"></div>
												<span class="text-gray-400 dark:text-gray-500">Disconnected</span>
											{/if}
										</div>
									{/each}
								</div>
							</div>
						</div>
					{/if}
				</div>
			{:else}
				{#if !error}
					<div class="py-12 text-center text-sm text-gray-500 dark:text-gray-400">
						{#if loading}
							Loading server status…
						{:else}
							No servers configured
						{/if}
					</div>
				{/if}
			{/each}
		</div>

		<!-- Footer -->
		<div class="border-t border-gray-200 bg-gray-50 px-5 py-3 text-xs text-gray-400 dark:border-gray-700 dark:bg-gray-800/50 dark:text-gray-500">
			Auto-refreshing every 2s
		</div>
	</div>
{/if}
