<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { fetchJSON } from '$lib/api';
	import { Info } from '@lucide/svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	interface AboutInfo {
		version: string;
		commit: string;
		build_date: string;
		go_version: string;
		local_ipv4: string;
		public_ipv4: string;
		public_ipv6: string;
		hostname: string;
		config_path: string;
		download_dir: string;
		complete_dir: string;
		admin_dir: string;
		log_dir: string;
		dirscan_dir: string;
		script_dir: string;
		par2_path: string;
		unrar_path: string;
		sevenz_path: string;
	}

	let info = $state<AboutInfo | null>(null);
	let error = $state('');
	let loading = $state(false);

	$effect(() => {
		if (open && !info) {
			loading = true;
			error = '';
			fetchJSON<{ about: AboutInfo }>('/api?mode=about&output=json')
				.then((res) => {
					info = res.about;
				})
				.catch((e) => {
					error = e instanceof Error ? e.message : String(e);
				})
				.finally(() => {
					loading = false;
				});
		}
	});

	interface InfoRow {
		label: string;
		value: string;
		mono?: boolean;
	}

	const sections = $derived.by((): { title: string; rows: InfoRow[] }[] => {
		if (!info) return [];
		return [
			{
				title: 'System',
				rows: [
					{ label: 'Version', value: info.version },
					...(info.commit && info.commit !== 'unknown'
						? [{ label: 'Commit', value: info.commit, mono: true }]
						: []),
					...(info.build_date && info.build_date !== 'unknown'
						? [{ label: 'Built', value: info.build_date }]
						: []),
					{ label: 'Go', value: info.go_version },
					{ label: 'Hostname', value: info.hostname },
					{ label: 'Local IP', value: info.local_ipv4 || '—' },
					{ label: 'Public IPv4', value: info.public_ipv4 || '(unavailable)' },
					{ label: 'Public IPv6', value: info.public_ipv6 || '(unavailable)' }
				]
			},
			{
				title: 'Directories',
				rows: [
					{ label: 'Config file', value: info.config_path, mono: true },
					{ label: 'Download', value: info.download_dir, mono: true },
					{ label: 'Complete', value: info.complete_dir, mono: true },
					{ label: 'Admin', value: info.admin_dir, mono: true },
					...(info.log_dir ? [{ label: 'Logs', value: info.log_dir, mono: true }] : []),
					...(info.dirscan_dir
						? [{ label: 'Dirscan', value: info.dirscan_dir, mono: true }]
						: []),
					...(info.script_dir
						? [{ label: 'Scripts', value: info.script_dir, mono: true }]
						: [])
				]
			},
			{
				title: 'External Programs',
				rows: [
					{
						label: 'par2',
						value: info.par2_path || 'not found',
						mono: !!info.par2_path
					},
					{
						label: 'unrar',
						value: info.unrar_path || 'not found',
						mono: !!info.unrar_path
					},
					{
						label: '7-zip',
						value: info.sevenz_path || 'not found',
						mono: !!info.sevenz_path
					}
				]
			}
		];
	});
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-m3-outline/20 bg-m3-surface text-m3-on-surface shadow-m3-3 outline-none"
		>
			<!-- Header -->
			<div class="border-b border-m3-outline/10 px-6 py-4">
				<Dialog.Title class="flex items-center gap-2.5 text-lg font-semibold tracking-tight text-m3-on-surface">
					<Info class="text-m3-primary size-6" />
					About GoNZBD
				</Dialog.Title>
			</div>

			<!-- Body -->
			<div class="max-h-[60vh] overflow-y-auto px-6 py-4">
				{#if loading}
					<div class="flex items-center justify-center py-10">
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
					</div>
				{:else if error}
					<div
						class="rounded-2xl border border-destructive/20 bg-destructive/10 px-4 py-3 text-sm text-destructive"
					>
						{error}
					</div>
				{:else if info}
					{#each sections as section, i (section.title)}
						{#if i > 0}
							<div class="my-4 h-px bg-m3-outline/10"></div>
						{/if}
						<h3
							class="mb-3 text-[11px] font-bold uppercase tracking-wider text-m3-primary"
						>
							{section.title}
						</h3>
						<dl class="space-y-2">
							{#each section.rows as row (row.label)}
								<div class="flex items-start gap-3">
									<dt
										class="w-24 shrink-0 text-xs font-semibold text-m3-on-surface-variant/80"
									>
										{row.label}
									</dt>
									<dd
										class="min-w-0 break-all text-sm {row.mono
											? 'font-mono text-xs'
											: ''} {row.value === 'not found'
											? 'text-amber-600 dark:text-amber-400'
											: 'text-m3-on-surface'}"
									>
										{row.value}
									</dd>
								</div>
							{/each}
						</dl>
					{/each}
				{/if}
			</div>

			<!-- Footer -->
			<div
				class="border-t border-m3-outline/10 px-6 py-4 text-center text-xs text-m3-on-surface-variant/70 font-medium"
			>
				GoNZBD — A Go reimplementation of SABnzbd
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
