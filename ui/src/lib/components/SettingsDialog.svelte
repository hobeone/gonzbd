<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Separator } from '$lib/components/ui/separator';
	import { Badge } from '$lib/components/ui/badge';
	import { setConfig, postAction } from '$lib/api';
	import ConfigInput from './config/ConfigInput.svelte';
	import ConfigSwitch from './config/ConfigSwitch.svelte';
	import ServerEditDialog from './config/ServerEditDialog.svelte';
	import CategoryEditDialog from './config/CategoryEditDialog.svelte';
	import SorterEditDialog from './config/SorterEditDialog.svelte';
	import ScheduleEditDialog from './config/ScheduleEditDialog.svelte';
	import RSSEditDialog from './config/RSSEditDialog.svelte';
	import type { ServerConfig, CategoryConfig, SorterConfig, ScheduleConfig, RSSFeedConfig } from '$lib/types';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let configData = $state<Record<string, any> | null>(null);
	let loading = $state(false);
	let saving = $state(false);
	let error = $state<string | null>(null);

	let activeSection = $state('general');
	let serverEditOpen = $state(false);
	let selectedServer = $state<ServerConfig | null>(null);

	let categoryEditOpen = $state(false);
	let selectedCategory = $state<CategoryConfig | null>(null);

	let sorterEditOpen = $state(false);
	let selectedSorter = $state<SorterConfig | null>(null);

	let scheduleEditOpen = $state(false);
	let selectedSchedule = $state<ScheduleConfig | null>(null);

	let rssEditOpen = $state(false);
	let selectedFeed = $state<RSSFeedConfig | null>(null);

	const sections = [
		{ id: 'general', label: 'General' },
		{ id: 'downloads', label: 'Downloads' },
		{ id: 'postproc', label: 'Post-Processing' },
		{ id: 'servers', label: 'Servers' },
		{ id: 'categories', label: 'Categories' },
		{ id: 'sorters', label: 'Sorters' },
		{ id: 'rss', label: 'RSS' },
		{ id: 'scheduling', label: 'Scheduling' }
	];

	$effect(() => {
		if (open && !configData && !loading) {
			fetchConfig();
		}
	});

	function fetchConfig() {
		loading = true;
		error = null;
		fetch('/api?mode=get_config&output=json')
			.then((res) => {
				if (!res.ok) throw new Error(`API ${res.status}: ${res.statusText}`);
				return res.json();
			})
			.then((data) => {
				configData = data.config ?? data;
			})
			.catch((e) => {
				error = e instanceof Error ? e.message : String(e);
			})
			.finally(() => {
				loading = false;
			});
	}

	function reloadConfig() {
		configData = null;
		fetchConfig();
	}

	function handleFieldUpdate(section: string, keyword: string, value: string | number | boolean) {
		if (!configData) return;
		const original = configData[section]?.[keyword];
		configData[section][keyword] = value;
		saving = true;
		setConfig(section, keyword, value)
			.catch((e) => {
				if (configData) configData[section][keyword] = original;
				error = `Failed to save ${keyword}: ${e instanceof Error ? e.message : String(e)}`;
			})
			.finally(() => {
				saving = false;
			});
	}

	function saveServer(s: ServerConfig) {
		if (!configData) return;
		const servers = [...(configData.servers ?? [])];
		const idx = servers.findIndex((srv: ServerConfig) => srv.name === s.name);
		if (idx !== -1) servers[idx] = s;
		else servers.push(s);
		configData = { ...configData, servers };
		persistAndReload('servers', servers);
	}

	function deleteServer(name: string) {
		if (!configData || !confirm(`Delete server "${name}"?`)) return;
		const servers = configData.servers.filter((s: ServerConfig) => s.name !== name);
		configData = { ...configData, servers };
		persistAndReload('servers', servers);
	}

	function saveCategory(c: CategoryConfig) {
		if (!configData) return;
		const categories = [...(configData.categories ?? [])];
		const idx = categories.findIndex((cat: CategoryConfig) => cat.name === c.name);
		if (idx !== -1) categories[idx] = c;
		else categories.push(c);
		configData = { ...configData, categories };
		persistAndReload('categories', categories);
	}

	function deleteCategory(name: string) {
		if (!configData || !confirm(`Delete category "${name}"?`)) return;
		const categories = configData.categories.filter((c: CategoryConfig) => c.name !== name);
		configData = { ...configData, categories };
		persistAndReload('categories', categories);
	}

	function saveSorter(s: SorterConfig) {
		if (!configData) return;
		const sorters = [...(configData.sorters ?? [])];
		const idx = sorters.findIndex((srv: SorterConfig) => srv.name === s.name);
		if (idx !== -1) sorters[idx] = s;
		else sorters.push(s);
		configData = { ...configData, sorters };
		persistAndReload('sorters', sorters);
	}

	function deleteSorter(name: string) {
		if (!configData || !confirm(`Delete sorter "${name}"?`)) return;
		const sorters = configData.sorters.filter((s: SorterConfig) => s.name !== name);
		configData = { ...configData, sorters };
		persistAndReload('sorters', sorters);
	}

	function saveSchedule(s: ScheduleConfig) {
		if (!configData) return;
		const schedules = [...(configData.schedules ?? [])];
		const idx = schedules.findIndex((sched: ScheduleConfig) => sched.name === s.name);
		if (idx !== -1) schedules[idx] = s;
		else schedules.push(s);
		configData = { ...configData, schedules };
		persistAndReload('schedules', schedules);
	}

	function deleteSchedule(name: string) {
		if (!configData || !confirm(`Delete schedule "${name}"?`)) return;
		const schedules = configData.schedules.filter((s: ScheduleConfig) => s.name !== name);
		configData = { ...configData, schedules };
		persistAndReload('schedules', schedules);
	}

	function saveRSSFeed(f: RSSFeedConfig) {
		if (!configData) return;
		const rss = [...(configData.rss ?? [])];
		const idx = rss.findIndex((feed: RSSFeedConfig) => feed.name === f.name);
		if (idx !== -1) rss[idx] = f;
		else rss.push(f);
		configData = { ...configData, rss };
		persistAndReload('rss', rss);
	}

	function deleteRSSFeed(name: string) {
		if (!configData || !confirm(`Delete RSS feed "${name}"?`)) return;
		const rss = configData.rss.filter((f: RSSFeedConfig) => f.name !== name);
		configData = { ...configData, rss };
		persistAndReload('rss', rss);
	}

	function persistAndReload(section: string, items: any[]) {
		saving = true;
		setConfig(section, '', JSON.stringify(items))
			.then(() => reloadConfig())
			.catch((e) => {
				error = `Failed to save ${section}: ${e instanceof Error ? e.message : String(e)}`;
				reloadConfig();
			})
			.finally(() => {
				saving = false;
			});
	}

	async function testServer(s: ServerConfig) {
		try {
			await postAction('config', {
				name: 'test_server',
				host: s.host,
				port: String(s.port),
				username: s.username,
				password: s.password,
				ssl: s.ssl ? '1' : '0',
				ssl_verify: String(s.ssl_verify)
			});
			alert('Connection successful!');
		} catch (e) {
			alert('Connection failed: ' + (e instanceof Error ? e.message : String(e)));
		}
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-50 flex h-[85vh] w-full max-w-4xl -translate-x-1/2 -translate-y-1/2 overflow-hidden rounded-lg border bg-white shadow-lg">
			<!-- Sidebar -->
			<aside class="w-64 shrink-0 border-r bg-gray-50/50 p-4">
				<Dialog.Title class="px-2 text-lg font-bold tracking-tight">Settings</Dialog.Title>
				<nav class="mt-6 space-y-1">
					{#each sections as section}
						<button
							onclick={() => (activeSection = section.id)}
							class="w-full rounded-md px-3 py-2 text-left text-sm font-medium transition-colors
							{activeSection === section.id ? 'bg-blue-100 text-blue-700' : 'text-gray-600 hover:bg-gray-100'}"
						>
							{section.label}
						</button>
					{/each}
				</nav>
			</aside>

			<!-- Main Content -->
			<div class="flex flex-1 flex-col overflow-hidden">
				<div class="flex-1 overflow-y-auto p-8">
					{#if loading}
						<div class="flex h-32 items-center justify-center text-sm text-gray-500">
							Loading configuration...
						</div>
					{:else if error}
						<div class="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
							{error}
						</div>
					{:else if configData}
							{#if activeSection === 'general'}
								<section class="space-y-6">
									<div>
										<h3 class="text-lg font-medium">General Settings</h3>
										<p class="text-sm text-muted-foreground">Server connectivity and basic daemon tuning.</p>
									</div>
									<Separator />
									<div class="divide-y divide-gray-100">
										<ConfigInput section="general" keyword="host" label="Host" value={configData.general.host} description="Host or IP to bind the HTTP server to." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="port" label="Port" type="number" value={configData.general.port} description="TCP port for the web interface." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="api_key" label="API Key" value={configData.general.api_key} description="Full API authentication key." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="nzb_key" label="NZB Key" value={configData.general.nzb_key} description="Key for NZB uploads only." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="download_dir" label="Download Directory" value={configData.general.download_dir} description="Path for incomplete downloads." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="complete_dir" label="Complete Directory" value={configData.general.complete_dir} description="Path for finished downloads." onupdate={handleFieldUpdate} />
										<ConfigInput section="general" keyword="log_level" label="Log Level" value={configData.general.log_level} description="Minimum level for logging (debug, info, warn, error)." onupdate={handleFieldUpdate} />
									</div>
								</section>
							{:else if activeSection === 'downloads'}
								<section class="space-y-6">
									<div>
										<h3 class="text-lg font-medium">Download Settings</h3>
										<p class="text-sm text-muted-foreground">Throttling, disk guards, and retry behavior.</p>
									</div>
									<Separator />
									<div class="divide-y divide-gray-100">
										<ConfigInput section="downloads" keyword="bandwidth_max" label="Maximum Bandwidth" value={configData.downloads.bandwidth_max} description="Absolute ceiling (e.g. 10M, 500K)." onupdate={handleFieldUpdate} />
										<ConfigInput section="downloads" keyword="min_free_space" label="Minimum Free Space" value={configData.downloads.min_free_space} description="Pause download if disk space drops below this (e.g. 1G)." onupdate={handleFieldUpdate} />
										<ConfigInput section="downloads" keyword="article_cache_size" label="Article Cache" value={configData.downloads.article_cache_size} description="In-memory cache size (e.g. 500M)." onupdate={handleFieldUpdate} />
										<ConfigInput section="downloads" keyword="max_art_tries" label="Article Retries" type="number" value={configData.downloads.max_art_tries} description="Max attempts across all servers per article." onupdate={handleFieldUpdate} />
										<ConfigSwitch section="downloads" keyword="pre_check" label="Pre-check article availability" value={configData.downloads.pre_check} description="STAT check before download (saves bandwidth)." onupdate={handleFieldUpdate} />
									</div>
								</section>
							{:else if activeSection === 'postproc'}
								<section class="space-y-6">
									<div>
										<h3 class="text-lg font-medium">Post-Processing</h3>
										<p class="text-sm text-muted-foreground">Archive extraction and par2 repair behavior.</p>
									</div>
									<Separator />
									<div class="divide-y divide-gray-100">
										<ConfigSwitch section="postproc" keyword="enable_unrar" label="Enable RAR extraction" value={configData.postproc.enable_unrar} onupdate={handleFieldUpdate} />
										<ConfigSwitch section="postproc" keyword="enable_7zip" label="Enable 7-Zip extraction" value={configData.postproc.enable_7zip} onupdate={handleFieldUpdate} />
										<ConfigSwitch section="postproc" keyword="direct_unpack" label="Direct Unpack" value={configData.postproc.direct_unpack} description="Extract files while still downloading." onupdate={handleFieldUpdate} />
										<ConfigSwitch section="postproc" keyword="enable_par_cleanup" label="Cleanup par2 files" value={configData.postproc.enable_par_cleanup} description="Delete verification files after successful repair." onupdate={handleFieldUpdate} />
										<ConfigInput section="postproc" keyword="unrar_command" label="UnRAR path" value={configData.postproc.unrar_command} onupdate={handleFieldUpdate} />
										<ConfigInput section="postproc" keyword="par2_command" label="par2 path" value={configData.postproc.par2_command} onupdate={handleFieldUpdate} />
										</div>
										</section>
						{:else if activeSection === 'servers'}
							<section class="space-y-6">
								<div class="flex items-center justify-between">
									<div>
										<h3 class="text-lg font-medium">Usenet Servers</h3>
										<p class="text-sm text-muted-foreground">Manage your NNTP server connections.</p>
									</div>
									<Button size="sm" onclick={() => { selectedServer = null; serverEditOpen = true; }}>+ Add Server</Button>
								</div>
								<Separator />
								
								<div class="space-y-4">
									{#if configData.servers.length === 0}
										<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
											No servers configured.
										</div>
									{:else}
										<div class="overflow-hidden rounded-md border">
											<table class="w-full text-left text-sm">
												<thead class="bg-gray-50 text-xs uppercase text-gray-500">
													<tr>
														<th class="px-4 py-2">Server / Connection</th>
														<th class="px-4 py-2">Details</th>
														<th class="px-4 py-2 text-right">Actions</th>
													</tr>
												</thead>
												<tbody class="divide-y">
													{#each configData.servers as server}
														<tr class={server.enable ? 'hover:bg-gray-50' : 'bg-gray-50/50 grayscale-[0.5]'}>
															<td class="px-4 py-3">
																<div class="flex items-center gap-2 font-medium">
																	{server.displayname || server.name}
																	{#if !server.enable}
																		<Badge variant="destructive" class="py-0 h-3.5 text-[9px] uppercase tracking-tighter opacity-70">Disabled</Badge>
																	{/if}
																</div>
																<div class="mt-0.5 font-mono text-[11px] text-gray-500">
																	{server.host}:{server.port}
																	{#if server.ssl}
																		<span class="ml-1.5 inline-flex items-center rounded bg-blue-50 px-1 py-0 text-[9px] font-bold text-blue-600 ring-1 ring-inset ring-blue-500/20">TLS</span>
																	{/if}
																</div>
															</td>
															<td class="px-4 py-3">
																<div class="flex items-center gap-3 text-[11px] text-gray-600">
																	<div class="flex items-center gap-1">
																		<span class="text-gray-400">User:</span>
																		<span class="truncate max-w-[80px] font-medium">{server.username || 'anonymous'}</span>
																	</div>
																	<div class="h-3 w-px bg-gray-200"></div>
																	<div class="flex items-center gap-1">
																		<span class="text-gray-400">Priority:</span>
																		<span class="font-bold">{server.priority}</span>
																	</div>
																	<div class="h-3 w-px bg-gray-200"></div>
																	<div class="flex items-center gap-1">
																		<span class="text-gray-400">Pool:</span>
																		<span>{server.connections} conns</span>
																	</div>
																</div>
															</td>
															<td class="px-4 py-3 text-right">
																<div class="flex justify-end gap-0.5">
																	<Button variant="ghost" size="xs" onclick={() => testServer(server)} title="Test connection">Test</Button>
																	<Button variant="ghost" size="xs" onclick={() => { selectedServer = server; serverEditOpen = true; }} title="Edit server">Edit</Button>
																	<Button variant="ghost" size="xs" class="text-red-600" onclick={() => deleteServer(server.name)} title="Delete server">Delete</Button>
																</div>
															</td>
														</tr>
													{/each}
												</tbody>
											</table>
										</div>
									{/if}
								</div>
							</section>
						{:else if activeSection === 'categories'}
							<section class="space-y-6">
								<div class="flex items-center justify-between">
									<div>
										<h3 class="text-lg font-medium">Categories</h3>
										<p class="text-sm text-muted-foreground">Define how different types of downloads are handled.</p>
									</div>
									<Button size="sm" onclick={() => { selectedCategory = null; categoryEditOpen = true; }}>+ Add Category</Button>
								</div>
								<Separator />
								
								<div class="space-y-4">
									<div class="overflow-hidden rounded-md border">
										<table class="w-full text-left text-sm">
											<thead class="bg-gray-50 text-xs uppercase text-gray-500">
												<tr>
													<th class="px-4 py-2">Name</th>
													<th class="px-4 py-2">Path</th>
													<th class="px-4 py-2">PP</th>
													<th class="px-4 py-2 text-right">Actions</th>
												</tr>
											</thead>
											<tbody class="divide-y">
												{#each configData.categories as cat}
													<tr>
														<td class="px-4 py-3 font-medium">{cat.name}</td>
														<td class="px-4 py-3 text-gray-600">{cat.dir || '(default)'}</td>
														<td class="px-4 py-3">
															<div class="flex gap-1">
																{#if cat.pp & 1}<Badge variant="outline" class="px-1 py-0 text-[10px]">R</Badge>{/if}
																{#if cat.pp & 2}<Badge variant="outline" class="px-1 py-0 text-[10px]">U</Badge>{/if}
																{#if cat.pp & 4}<Badge variant="outline" class="px-1 py-0 text-[10px]">D</Badge>{/if}
															</div>
														</td>
														<td class="px-4 py-3 text-right">
															<div class="flex justify-end gap-1">
																<Button variant="ghost" size="xs" onclick={() => { selectedCategory = cat; categoryEditOpen = true; }}>Edit</Button>
																<Button variant="ghost" size="xs" class="text-red-600" disabled={cat.name === '*' || cat.name === 'Default'} onclick={() => deleteCategory(cat.name)}>Delete</Button>
															</div>
														</td>
													</tr>
												{/each}
											</tbody>
										</table>
									</div>
								</div>
							</section>
						{:else if activeSection === 'sorters'}
							<section class="space-y-6">
								<div class="flex items-center justify-between">
									<div>
										<h3 class="text-lg font-medium">Sorters</h3>
										<p class="text-sm text-muted-foreground">Automated file renaming based on media metadata.</p>
									</div>
									<Button size="sm" onclick={() => { selectedSorter = null; sorterEditOpen = true; }}>+ Add Sorter</Button>
								</div>
								<Separator />
								
								<div class="space-y-4">
									{#if configData.sorters.length === 0}
										<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
											No sorters configured.
										</div>
									{:else}
										<div class="overflow-hidden rounded-md border">
											<table class="w-full text-left text-sm">
												<thead class="bg-gray-50 text-xs uppercase text-gray-500">
													<tr>
														<th class="px-4 py-2">Name</th>
														<th class="px-4 py-2">Template</th>
														<th class="px-4 py-2 text-right">Actions</th>
													</tr>
												</thead>
												<tbody class="divide-y">
													{#each configData.sorters as sorter}
														<tr class={sorter.is_active ? '' : 'opacity-50'}>
															<td class="px-4 py-3 font-medium">{sorter.name}</td>
															<td class="px-4 py-3 font-mono text-xs text-gray-600 truncate max-w-xs">{sorter.sort_string}</td>
															<td class="px-4 py-3 text-right">
																<div class="flex justify-end gap-1">
																	<Button variant="ghost" size="xs" onclick={() => { selectedSorter = sorter; sorterEditOpen = true; }}>Edit</Button>
																	<Button variant="ghost" size="xs" class="text-red-600" onclick={() => deleteSorter(sorter.name)}>Delete</Button>
																</div>
															</td>
														</tr>
													{/each}
												</tbody>
											</table>
										</div>
									{/if}
								</div>
							</section>
						{:else if activeSection === 'rss'}
							<section class="space-y-6">
								<div class="flex items-center justify-between">
									<div>
										<h3 class="text-lg font-medium">RSS Feeds</h3>
										<p class="text-sm text-muted-foreground">Automated downloads from indexers.</p>
									</div>
									<Button size="sm" onclick={() => { selectedFeed = null; rssEditOpen = true; }}>+ Add Feed</Button>
								</div>
								<Separator />
								
								<div class="space-y-4">
									{#if configData.rss.length === 0}
										<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
											No feeds configured.
										</div>
									{:else}
										<div class="overflow-hidden rounded-md border">
											<table class="w-full text-left text-sm">
												<thead class="bg-gray-50 text-xs uppercase text-gray-500">
													<tr>
														<th class="px-4 py-2">Name</th>
														<th class="px-4 py-2">URI</th>
														<th class="px-4 py-2 text-right">Actions</th>
													</tr>
												</thead>
												<tbody class="divide-y">
													{#each configData.rss as feed}
														<tr class={feed.enable ? '' : 'opacity-50'}>
															<td class="px-4 py-3 font-medium">{feed.name}</td>
															<td class="px-4 py-3 text-gray-600 truncate max-w-xs">{feed.uri}</td>
															<td class="px-4 py-3 text-right">
																<div class="flex justify-end gap-1">
																	<Button variant="ghost" size="xs" onclick={() => { selectedFeed = feed; rssEditOpen = true; }}>Edit</Button>
																	<Button variant="ghost" size="xs" class="text-red-600" onclick={() => deleteRSSFeed(feed.name)}>Delete</Button>
																</div>
															</td>
														</tr>
													{/each}
												</tbody>
											</table>
										</div>
									{/if}
								</div>
							</section>
						{:else if activeSection === 'scheduling'}
							<section class="space-y-6">
								<div class="flex items-center justify-between">
									<div>
										<h3 class="text-lg font-medium">Schedules</h3>
										<p class="text-sm text-muted-foreground">Automated actions based on time.</p>
									</div>
									<Button size="sm" onclick={() => { selectedSchedule = null; scheduleEditOpen = true; }}>+ Add Schedule</Button>
								</div>
								<Separator />
								
								<div class="space-y-4">
									{#if configData.schedules.length === 0}
										<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
											No schedules configured.
										</div>
									{:else}
										<div class="overflow-hidden rounded-md border">
											<table class="w-full text-left text-sm">
												<thead class="bg-gray-50 text-xs uppercase text-gray-500">
													<tr>
														<th class="px-4 py-2">Name</th>
														<th class="px-4 py-2">Time</th>
														<th class="px-4 py-2">Action</th>
														<th class="px-4 py-2 text-right">Actions</th>
													</tr>
												</thead>
												<tbody class="divide-y">
													{#each configData.schedules as sched}
														<tr class={sched.enabled ? '' : 'opacity-50'}>
															<td class="px-4 py-3 font-medium">{sched.name}</td>
															<td class="px-4 py-3 text-gray-600 font-mono text-xs">{sched.hour}:{sched.minute} ({sched.dayofweek})</td>
															<td class="px-4 py-3 uppercase text-xs font-bold">{sched.action}</td>
															<td class="px-4 py-3 text-right">
																<div class="flex justify-end gap-1">
																	<Button variant="ghost" size="xs" onclick={() => { selectedSchedule = sched; scheduleEditOpen = true; }}>Edit</Button>
																	<Button variant="ghost" size="xs" class="text-red-600" onclick={() => deleteSchedule(sched.name)}>Delete</Button>
																</div>
															</td>
														</tr>
													{/each}
												</tbody>
											</table>
										</div>
									{/if}
								</div>
							</section>
						{/if}
					{/if}
				</div>

				<!-- Footer -->
				<footer class="flex items-center justify-between border-t bg-gray-50 px-8 py-4">
					<div class="text-xs text-muted-foreground">
						{#if saving}
							<span class="flex items-center gap-2">
								<svg class="h-3 w-3 animate-spin" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" fill="none"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
								Saving changes...
							</span>
						{:else}
							Changes are saved automatically.
						{/if}
					</div>
					<Button variant="outline" onclick={() => (open = false)}>Close</Button>
				</footer>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>

<ServerEditDialog
	bind:open={serverEditOpen}
	server={selectedServer}
	onsave={saveServer}
/>

<CategoryEditDialog
	bind:open={categoryEditOpen}
	category={selectedCategory}
	onsave={saveCategory}
/>

<SorterEditDialog
	bind:open={sorterEditOpen}
	sorter={selectedSorter}
	onsave={saveSorter}
/>

<ScheduleEditDialog
	bind:open={scheduleEditOpen}
	schedule={selectedSchedule}
	onsave={saveSchedule}
/>

<RSSEditDialog
	bind:open={rssEditOpen}
	feed={selectedFeed}
	onsave={saveRSSFeed}
/>
