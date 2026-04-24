<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { setConfig, postAction } from '$lib/api';
	import GeneralSection from './config/GeneralSection.svelte';
	import DownloadsSection from './config/DownloadsSection.svelte';
	import PostProcSection from './config/PostProcSection.svelte';
	import ServersSection from './config/ServersSection.svelte';
	import CategoriesSection from './config/CategoriesSection.svelte';
	import SortersSection from './config/SortersSection.svelte';
	import RSSSection from './config/RSSSection.svelte';
	import SchedulingSection from './config/SchedulingSection.svelte';
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

	function saveServer(s: ServerConfig, originalName?: string) {
		if (!configData) return;
		const servers = [...(configData.servers ?? [])];
		const lookupName = originalName || s.name;
		const idx = servers.findIndex((srv: ServerConfig) => srv.name === lookupName);
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
							<GeneralSection {configData} onFieldUpdate={handleFieldUpdate} />
						{:else if activeSection === 'downloads'}
							<DownloadsSection {configData} onFieldUpdate={handleFieldUpdate} />
						{:else if activeSection === 'postproc'}
							<PostProcSection {configData} onFieldUpdate={handleFieldUpdate} />
						{:else if activeSection === 'servers'}
							<ServersSection
								{configData}
								onAddServer={() => { selectedServer = null; serverEditOpen = true; }}
								onEditServer={(s) => { selectedServer = s; serverEditOpen = true; }}
								onDeleteServer={deleteServer}
								onTestServer={testServer}
							/>
						{:else if activeSection === 'categories'}
							<CategoriesSection
								{configData}
								onAddCategory={() => { selectedCategory = null; categoryEditOpen = true; }}
								onEditCategory={(c) => { selectedCategory = c; categoryEditOpen = true; }}
								onDeleteCategory={deleteCategory}
							/>
						{:else if activeSection === 'sorters'}
							<SortersSection
								{configData}
								onAddSorter={() => { selectedSorter = null; sorterEditOpen = true; }}
								onEditSorter={(s) => { selectedSorter = s; sorterEditOpen = true; }}
								onDeleteSorter={deleteSorter}
							/>
						{:else if activeSection === 'rss'}
							<RSSSection
								{configData}
								onAddFeed={() => { selectedFeed = null; rssEditOpen = true; }}
								onEditFeed={(f) => { selectedFeed = f; rssEditOpen = true; }}
								onDeleteFeed={deleteRSSFeed}
							/>
						{:else if activeSection === 'scheduling'}
							<SchedulingSection
								{configData}
								onAddSchedule={() => { selectedSchedule = null; scheduleEditOpen = true; }}
								onEditSchedule={(s) => { selectedSchedule = s; scheduleEditOpen = true; }}
								onDeleteSchedule={deleteSchedule}
							/>
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
