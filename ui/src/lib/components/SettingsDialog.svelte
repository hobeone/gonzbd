<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { setConfig, postAction, fetchConfig } from '$lib/api';
	import SettingsIcon from '@lucide/svelte/icons/settings';
	import AlertCircle from '@lucide/svelte/icons/alert-circle';
	import X from '@lucide/svelte/icons/x';
	import GeneralSection from './config/GeneralSection.svelte';
	import DownloadsSection from './config/DownloadsSection.svelte';
	import PostProcSection from './config/PostProcSection.svelte';
	import ServersSection from './config/ServersSection.svelte';
	import CategoriesSection from './config/CategoriesSection.svelte';
	import ServerEditDialog from './config/ServerEditDialog.svelte';
	import CategoryEditDialog from './config/CategoryEditDialog.svelte';
	import type { ServerConfig, CategoryConfig } from '$lib/types';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let configData = $state<Record<string, any> | null>(null);
	let loading = $state(false);
	let saving = $state(false);
	let error = $state<string | null>(null);
	let dirtyFields = $state<{ section: string; keyword: string; value: string | number | boolean }[]>([]);

	let activeSection = $state('general');
	let serverEditOpen = $state(false);
	let selectedServer = $state<ServerConfig | null>(null);

	let categoryEditOpen = $state(false);
	let selectedCategory = $state<CategoryConfig | null>(null);


	const sections = [
		{ id: 'general', label: 'General' },
		{ id: 'downloads', label: 'Downloads' },
		{ id: 'postproc', label: 'Post-Processing' },
		{ id: 'servers', label: 'Servers' },
		{ id: 'categories', label: 'Categories' }
	];

	$effect(() => {
		if (open && !configData && !loading) {
			loadConfig();
		}
	});

	function loadConfig() {
		loading = true;
		error = null;
		fetchConfig()
			.then((res) => {
				const cfg: Record<string, any> = res.config ?? res;
				// The API remaps "general" → "misc" for SABnzbd compatibility
				// (Sonarr reads config.misc.complete_dir). Reverse-map it so
				// the UI can reference configData.general.* consistently.
				if (cfg.misc && !cfg.general) {
					cfg.general = cfg.misc;
					delete cfg.misc;
				}
				configData = cfg;
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
		dirtyFields = [];
		error = null;
		loadConfig();
	}

	function handleFieldUpdate(section: string, keyword: string, value: string | number | boolean) {
		if (!configData) return;
		configData[section][keyword] = value;

		// Track as dirty field
		const idx = dirtyFields.findIndex((f) => f.section === section && f.keyword === keyword);
		if (idx !== -1) {
			dirtyFields[idx].value = value;
		} else {
			dirtyFields.push({ section, keyword, value });
		}
	}

	async function saveAll() {
		if (dirtyFields.length === 0) return;
		saving = true;
		error = null;

		let currentField = '';
		try {
			// Save fields sequentially to handle errors properly
			for (const field of [...dirtyFields]) {
				await setConfig(field.section, field.keyword, field.value);
				// Remove from dirty if successful
				dirtyFields = dirtyFields.filter((f) => f !== field);
			}
			// All saved successfully
			dirtyFields = [];
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			saving = false;
		}
	}

	function discardChanges() {
		if (dirtyFields.length > 0) {
			reloadConfig();
		}
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
		if (!configData) return;
		const servers = configData.servers.filter((s: ServerConfig) => s.name !== name);
		configData = { ...configData, servers };
		persistAndReload('servers', servers);
	}

	function toggleServer(s: ServerConfig, enabled: boolean) {
		saveServer({ ...s, enable: enabled });
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

	function persistAndReload(section: string, items: any[]) {
		saving = true;
		setConfig(section, '', JSON.stringify(items))
			.then(() => {
				error = null;
				reloadConfig();
			})
			.catch((e) => {
				error = `Failed to save ${section}: ${e instanceof Error ? e.message : String(e)}`;
			})
			.finally(() => {
				saving = false;
			});
	}

	async function testServer(s: ServerConfig): Promise<{ ok: boolean; message: string }> {
		try {
			const res = await postAction('config', {
				name: 'test_server',
				host: s.host,
				port: String(s.port),
				username: s.username,
				password: s.password,
				ssl: s.ssl ? '1' : '0',
				ssl_verify: String(s.ssl_verify)
			});
			const r = (res as any).result;
			if (r && typeof r.passed === 'boolean') {
				return { ok: r.passed, message: r.message || (r.passed ? 'Connection successful!' : 'Connection failed.') };
			}
			return { ok: true, message: 'Connection successful!' };
		} catch (e) {
			return { ok: false, message: e instanceof Error ? e.message : String(e) };
		}
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-50 flex h-[85vh] w-full max-w-4xl -translate-x-1/2 -translate-y-1/2 overflow-hidden rounded-3xl border border-m3-outline/20 bg-m3-surface text-m3-on-surface shadow-m3-3 outline-none">
			<!-- Sidebar -->
			<aside class="w-64 shrink-0 border-r border-m3-outline/10 bg-m3-surface-variant/30 p-5">
				<Dialog.Title class="px-2 text-base font-bold tracking-tight text-foreground flex items-center gap-2 select-none">
					<SettingsIcon class="size-5 text-primary" />
					Settings
				</Dialog.Title>
				<nav class="mt-6 space-y-1">
					{#each sections as section (section.id)}
						<button
							onclick={() => (activeSection = section.id)}
							class="w-full rounded-full px-4 py-2 text-left text-xs font-bold tracking-wide transition-all
							{activeSection === section.id ? 'bg-primary text-primary-foreground shadow-xs' : 'text-muted-foreground hover:bg-muted hover:text-foreground'}"
						>
							{section.label}
						</button>
					{/each}
				</nav>
			</aside>

			<!-- Main Content -->
			<div class="flex flex-1 flex-col overflow-hidden">
				<div class="flex-1 overflow-y-auto p-8">
					{#if error}
						<div class="mb-6 flex items-center justify-between rounded-2xl border border-destructive/30 bg-destructive/10 p-4 text-xs text-destructive font-semibold">
							<div class="flex items-center gap-2">
								<AlertCircle class="size-4 shrink-0" />
								<span>{error}</span>
							</div>
							<button onclick={() => (error = null)} class="text-destructive hover:opacity-80" aria-label="Dismiss error">
								<X class="size-4 shrink-0" />
							</button>
						</div>
					{/if}

					{#if loading}
						<div class="flex h-32 flex-col gap-3 items-center justify-center text-sm text-m3-on-surface-variant/80">
							<svg
								class="size-8 animate-spin text-m3-primary"
								xmlns="http://www.w3.org/2000/svg"
								fill="none"
								viewBox="0 0 24 24"
							>
								<circle
									class="opacity-25"
									cx="12"
									cy="12"
									r="10"
									stroke="currentColor"
									stroke-width="4"
								></circle>
								<path
									class="opacity-75"
									fill="currentColor"
									d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z"
								></path>
							</svg>
							<span>Loading configuration...</span>
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
								onTestServer={testServer}
								onToggleServer={toggleServer}
							/>
						{:else if activeSection === 'categories'}
							<CategoriesSection
								{configData}
								onAddCategory={() => { selectedCategory = null; categoryEditOpen = true; }}
								onEditCategory={(c) => { selectedCategory = c; categoryEditOpen = true; }}
								onDeleteCategory={deleteCategory}
							/>
						{/if}
					{/if}
				</div>

				<!-- Footer -->
				<footer class="flex items-center justify-between border-t border-m3-outline/10 bg-m3-surface-variant/20 px-8 py-4">
					<div class="text-xs text-m3-on-surface-variant">
						{#if saving}
							<span class="flex items-center gap-2 font-medium">
								<svg class="h-4 w-4 animate-spin text-m3-primary" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" fill="none"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path></svg>
								Saving changes...
							</span>
						{:else if dirtyFields.length > 0}
							<span class="font-bold text-amber-600 dark:text-amber-400">{dirtyFields.length} unsaved changes</span>
						{:else}
							All changes are synced with the server.
						{/if}
					</div>
					<div class="flex gap-3">
						{#if dirtyFields.length > 0}
							<Button variant="ghost" class="rounded-full px-5" onclick={discardChanges} disabled={saving}>Discard</Button>
							<Button class="bg-m3-primary text-m3-on-primary hover:bg-m3-primary/90 rounded-full px-5" onclick={saveAll} disabled={saving}>Save Changes</Button>
						{/if}
						<Button
							variant="outline"
							class="rounded-full px-5 border-m3-outline text-m3-on-surface hover:bg-m3-surface-variant/50"
							onclick={() => {
								if (dirtyFields.length > 0 && !confirm('You have unsaved changes. Close anyway?')) return;
								open = false;
							}}
						>
							Close
						</Button>
					</div>
				</footer>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>

<ServerEditDialog
	bind:open={serverEditOpen}
	server={selectedServer}
	onsave={saveServer}
	ondelete={deleteServer}
/>

<CategoryEditDialog
	bind:open={categoryEditOpen}
	category={selectedCategory}
	onsave={saveCategory}
/>

