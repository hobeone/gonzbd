<script lang="ts">
	import { fetchConfig, setConfig, postAction } from '$lib/api';
	import { getServerStats } from '$lib/stores/queue.svelte';
	import type { ServerSnapshot, ConnSnapshot, ServerConfig } from '$lib/types';
	import { formatSize as formatBytes, formatSpeed as formatBps } from '$lib/utils';
	import { Server, X, ChevronDown, Settings, AlertTriangle } from '@lucide/svelte';
	import ServerEditDialog from './config/ServerEditDialog.svelte';

	let {
		open = $bindable(false)
	}: {
		open?: boolean;
	} = $props();

	let error = $state('');
	let expandedServers = $state<Set<string>>(new Set());

	let editOpen = $state(false);
	let editServer = $state<ServerConfig | null>(null);
	let allServerConfigs = $state<ServerConfig[]>([]);
	let editLoadingFor = $state<string | null>(null);

	// Server stats come from the queue store's metrics WS event — no polling needed.
	let servers = $derived(getServerStats());

	async function openServerSettings(name: string) {
		editLoadingFor = name;
		try {
			const cfg = await fetchConfig();
			const list: ServerConfig[] = cfg.config?.servers ?? [];
			allServerConfigs = list;
			const match = list.find((s) => s.name === name) ?? null;
			if (match) {
				editServer = match;
				editOpen = true;
			} else {
				error = `Server "${name}" not found in config`;
			}
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to load server config';
		} finally {
			editLoadingFor = null;
		}
	}

	async function saveServer(updated: ServerConfig, originalName?: string) {
		const lookup = originalName || updated.name;
		const next = [...allServerConfigs];
		const idx = next.findIndex((s) => s.name === lookup);
		if (idx === -1) next.push(updated);
		else next[idx] = updated;
		try {
			await setConfig('servers', '', JSON.stringify(next));
			allServerConfigs = next;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to save server';
		}
	}

	async function deleteServer(name: string) {
		const next = allServerConfigs.filter((s) => s.name !== name);
		try {
			await setConfig('servers', '', JSON.stringify(next));
			allServerConfigs = next;
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to delete server';
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

	function formatPenalty(unixTs: number): string {
		if (!unixTs) return '';
		const remaining = Math.max(0, unixTs - Math.floor(Date.now() / 1000));
		if (remaining === 0) return '';
		const mins = Math.floor(remaining / 60);
		const secs = remaining % 60;
		return mins > 0 ? `${mins}m ${secs}s` : `${secs}s`;
	}

	async function unblockServer(name: string) {
		try {
			await postAction('status', { name: 'unblock_server', value: name });
		} catch (e) {
			error = e instanceof Error ? e.message : 'Failed to unblock server';
		}
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
</script>

{#if open}
	<!-- Backdrop -->
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<!-- svelte-ignore a11y_no_static_element_interactions -->
	<div
		class="fixed inset-0 z-40 bg-black/50 backdrop-blur-xs transition-opacity animate-in fade-in"
		onclick={() => (open = false)}
	></div>

	<!-- Drawer -->
	<div
		class="fixed top-0 right-0 z-50 flex h-full w-full max-w-lg flex-col border-l border-m3-outline/10 bg-m3-surface text-m3-on-surface rounded-l-3xl shadow-m3-3 outline-none"
	>
		<!-- Header -->
		<div class="flex items-center justify-between border-b border-border/40 px-6 py-4">
			<div class="flex items-center gap-3">
				<Server class="size-5 text-primary" />
				<h2 class="text-base font-bold tracking-tight text-foreground">Server Status</h2>
			</div>
			<button
				onclick={() => (open = false)}
				aria-label="Close"
				class="rounded-full p-1.5 text-muted-foreground hover:bg-muted hover:text-foreground transition-colors"
			>
				<X class="size-4" />
			</button>
		</div>

		<!-- Aggregate stats bar -->
		<div class="grid grid-cols-3 gap-3 border-b border-m3-outline/10 bg-m3-surface-variant/20 px-6 py-3.5">
			<div class="text-center">
				<div class="text-xs font-semibold uppercase tracking-wider text-m3-primary">Speed</div>
				<div class="mt-0.5 text-lg font-bold text-m3-on-surface">{formatBps(totalBps())}</div>
			</div>
			<div class="text-center">
				<div class="text-xs font-semibold uppercase tracking-wider text-m3-primary">Active</div>
				<div class="mt-0.5 text-lg font-bold text-m3-on-surface">{totalActiveConns()} / {totalMaxConns()}</div>
			</div>
			<div class="text-center">
				<div class="text-xs font-semibold uppercase tracking-wider text-m3-primary">Servers</div>
				<div class="mt-0.5 text-lg font-bold text-m3-on-surface">{servers.length}</div>
			</div>
		</div>

		<!-- Server list -->
		<div class="flex-1 overflow-y-auto p-5 space-y-4">
			{#if error}
				<div class="rounded-2xl border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive font-semibold">
					{error}
				</div>
			{/if}

			{#each servers as server (server.name)}
				{@const penalty = formatPenalty(server.penalty_until)}
				{@const isExpanded = expandedServers.has(server.name)}
				{@const aggrBps = totalBps()}
				{@const speedPct = aggrBps > 0 ? (server.bps / aggrBps) * 100 : 0}

				<div class="overflow-hidden rounded-2xl border border-m3-outline/20 bg-m3-surface-variant/10">
					<!-- Server header -->
					<div class="flex items-stretch hover:bg-m3-surface-variant/20 transition-colors">
						<button
							onclick={() => toggleExpanded(server.name)}
							class="flex flex-1 min-w-0 items-center gap-3.5 px-4 py-3 text-left"
						>
							<!-- Status dot -->
							<div class="flex-shrink-0">
								{#if !server.enabled}
									<div class="size-2.5 rounded-full bg-m3-on-surface-variant/40" title="Disabled"></div>
								{:else if penalty}
									<div class="size-2.5 rounded-full bg-amber-500 animate-pulse" title="Penalized"></div>
								{:else if server.active}
									<div class="size-2.5 rounded-full bg-emerald-500" title="Active"></div>
								{:else}
									<div class="size-2.5 rounded-full bg-destructive" title="Inactive"></div>
								{/if}
							</div>

							<!-- Server info -->
							<div class="min-w-0 flex-1">
								<div class="flex items-center gap-2">
									<span class="truncate font-semibold text-m3-on-surface text-sm">{server.name}</span>
									{#if server.ssl}
										<span class="rounded bg-green-500/10 px-1.5 py-0.5 text-[9px] font-bold uppercase text-green-600 dark:text-green-400">SSL</span>
									{/if}
									{#if server.optional}
										<span class="rounded bg-m3-primary-container px-1.5 py-0.5 text-[9px] font-bold uppercase text-m3-on-primary-container">Backup</span>
									{/if}
									<span class="text-[10px] font-bold text-m3-on-surface-variant/80">P{server.priority}</span>
								</div>
								<div class="mt-0.5 text-xs text-m3-on-surface-variant/80 font-medium">
									{server.host}:{server.port}
								</div>
							</div>

							<!-- Speed + conn count -->
							<div class="flex-shrink-0 text-right">
								<div class="text-sm font-bold text-m3-on-surface">{formatBps(server.bps)}</div>
								<div class="text-xs text-m3-on-surface-variant/85 font-medium">
									{server.active_conns}/{server.max_connections} conns
								</div>
							</div>

							<!-- Expand arrow -->
							<ChevronDown class="size-4 text-muted-foreground transition-transform duration-200 {isExpanded ? 'rotate-180' : ''}" />
						</button>

						<!-- Settings (gear) button -->
						<button
							onclick={() => openServerSettings(server.name)}
							disabled={editLoadingFor === server.name}
							title="Server settings"
							aria-label="Edit {server.name} settings"
							class="flex flex-shrink-0 items-center justify-center px-4 text-muted-foreground hover:text-primary transition-colors disabled:opacity-50"
						>
							{#if editLoadingFor === server.name}
								<svg class="size-4 animate-spin text-primary" viewBox="0 0 24 24" fill="none">
									<circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
									<path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
								</svg>
							{:else}
								<Settings class="size-4" />
							{/if}
						</button>
					</div>

					<!-- Speed proportion bar -->
					{#if server.enabled && aggrBps > 0}
						<div class="h-[3px] bg-border/40">
							<div
								class="h-full bg-primary transition-all duration-300"
								style="width: {Math.max(speedPct, 1)}%"
							></div>
						</div>
					{/if}

					<!-- Expanded details -->
					{#if isExpanded}
						<div class="border-t border-border/40 bg-muted/20">
							<!-- Stats -->
							<div class="grid grid-cols-2 gap-x-4 gap-y-1.5 px-4 py-3 text-xs">
								<div>
									<span class="text-muted-foreground font-medium">Total Downloaded:</span>
									<span class="ml-1 font-mono font-semibold text-foreground">{formatBytes(server.total_bytes)}</span>
								</div>
								<div>
									<span class="text-muted-foreground font-medium">Pipelining:</span>
									<span class="ml-1 font-mono font-semibold text-foreground">{server.pipelining || 1}</span>
								</div>
							</div>

							{#if penalty}
								<div class="mx-4 mb-3 flex items-center gap-2 rounded-2xl border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs">
									<AlertTriangle class="size-3.5 text-amber-500" />
									<span class="font-semibold text-amber-600 dark:text-amber-400">Penalized — {penalty} remaining</span>
									<button
										onclick={() => unblockServer(server.name)}
										class="ml-auto rounded-full bg-amber-500 text-white px-2.5 py-0.5 text-[10px] font-bold hover:bg-amber-400"
										title="Clear penalty and retry immediately"
									>
										Unblock
									</button>
								</div>
							{/if}

							{#if !server.enabled}
								<div class="mx-4 mb-3 flex items-center gap-2 rounded-2xl bg-m3-surface-variant/40 px-3 py-2 text-xs">
									<span class="font-semibold text-m3-on-surface-variant/70">Server disabled in configuration</span>
								</div>
							{/if}

							<!-- Per-connection table -->
							<div class="px-4 pb-3">
								<div class="text-[10px] font-bold uppercase tracking-wider text-m3-primary mb-2">
									Connections
								</div>
								<div class="space-y-1">
									{#each server.connections.toSorted((a, b) => a.index - b.index) as conn (conn.index)}
										<div class="flex items-center gap-2 rounded-lg px-2.5 py-1.5 text-xs {conn.article_id ? 'bg-m3-surface/50 shadow-m3-1' : 'bg-transparent'}">
											<span class="w-6 flex-shrink-0 font-mono text-m3-on-surface-variant/70">#{conn.index}</span>
											{#if conn.article_id}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-emerald-500 animate-pulse"></div>
												<span class="min-w-0 flex-1 truncate text-m3-on-surface font-medium" title={conn.subject || conn.article_id}>
													{conn.subject || conn.article_id}
												</span>
												<span class="flex-shrink-0 text-m3-on-surface-variant/80 font-medium">{formatBytes(conn.bytes)}</span>
												<span class="flex-shrink-0 font-mono text-m3-on-surface-variant/70">{connDuration(conn)}</span>
											{:else if conn.connected}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-m3-primary/60"></div>
												<span class="text-m3-on-surface-variant/70">Idle</span>
											{:else}
												<div class="size-1.5 flex-shrink-0 rounded-full bg-m3-on-surface-variant/30"></div>
												<span class="text-m3-on-surface-variant/50">Disconnected</span>
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
					<div class="py-12 text-center text-sm text-m3-on-surface-variant/70 font-semibold">
						No servers configured
					</div>
				{/if}
			{/each}
		</div>

		<!-- Footer -->
		<div class="border-t border-m3-outline/10 bg-m3-surface-variant/20 px-6 py-4 text-xs text-m3-on-surface-variant/80 font-medium">
			Auto-updating via WebSocket
		</div>
	</div>

	<ServerEditDialog
		bind:open={editOpen}
		server={editServer}
		onsave={saveServer}
		ondelete={deleteServer}
	/>
{/if}
