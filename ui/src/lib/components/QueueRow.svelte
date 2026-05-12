<script lang="ts">
	import type { QueueSlot, QueueFile } from '$lib/types';
	import { untrack } from 'svelte';
	import { Progress } from '$lib/components/ui/progress';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';
	import { fetchQueueJobDetail, postAction } from '$lib/api';
	import { subscribeWS } from '$lib/stores/websocket.svelte';
	import { cn, formatSize as formatBytes, formatETA } from '$lib/utils';

	const PP_LABELS: Record<string, string> = {
		'0': 'Download',
		'1': '+Repair',
		'2': '+Unpack',
		'3': '+Delete',
	};

	function ppLabel(pp: string): string {
		return PP_LABELS[pp] ?? `PP${pp}`;
	}

	let { slot, onremove }: { slot: QueueSlot; onremove: () => void } = $props();

	let acting = $state(false);
	let expanded = $state(false);

	// Per-file detail: only fetched while the drawer is open. Refetched
	// on queue_updated events (throttled) so per-file percent updates
	// while the row is expanded; cleared when the drawer closes.
	let files = $state<QueueFile[] | null>(null);
	let filesLoading = $state(false);
	let filesError = $state<string | null>(null);
	// 2 s — at 1 s the drawer redraws so often it visibly judders on
	// large file lists; 2 s feels live without thrashing.
	const FILES_REFRESH_MS = 2000;
	let lastFilesFetch = 0;
	let pendingFilesRefresh: ReturnType<typeof setTimeout> | null = null;

	// Real-time postproc output lines from subprocess streaming.
	const MAX_OUTPUT_LINES = 200;
	let outputLines = $state<string[]>([]);
	let outputLogEl: HTMLDivElement | undefined = $state();

	let percentage = $derived(parseFloat(slot.percentage) || 0);
	let isPaused = $derived(slot.status === 'Paused');
	let isPostProc = $derived(
		['Verifying', 'Repairing', 'Extracting', 'Moving', 'Running'].includes(slot.status)
	);
	let isActive = $derived(
		slot.status !== 'Queued' && slot.status !== 'Paused' && slot.status !== 'Idle'
	);
	let hasFailed = $derived(slot.failed_bytes > 0);
	let etaText = $derived(formatETA(slot.eta_seconds ?? 0));
	let isDownloading = $derived(slot.current_stage === 'download');

	/**
	 * Apply an incoming files array to the reactive `files` state.
	 *
	 * On the first load (`files == null`) or when the shape changes
	 * (length differs, or any name at the same index differs — order
	 * is normally NZB-stable but we don't want to assume), fall back
	 * to a full assignment.
	 *
	 * Otherwise, mutate fields in place so Svelte 5's deep $state
	 * reactivity only re-renders the cells that actually changed.
	 * Most refreshes only touch bytes_downloaded (and occasionally
	 * `state`) on the actively-downloading file; in-place updates
	 * leave the other 99 rows untouched.
	 */
	function applyFilesUpdate(next: QueueFile[]) {
		const cur = files;
		if (!cur || cur.length !== next.length) {
			files = next;
			return;
		}
		// Detect order/identity change — if any name has shifted, the
		// in-place mapping isn't valid; fall back to replacement.
		for (let i = 0; i < next.length; i++) {
			if (cur[i].name !== next[i].name) {
				files = next;
				return;
			}
		}
		// Same shape, same order. Mutate only the fields that moved.
		for (let i = 0; i < next.length; i++) {
			const c = cur[i];
			const n = next[i];
			if (c.bytes !== n.bytes) c.bytes = n.bytes;
			if (c.bytes_downloaded !== n.bytes_downloaded) c.bytes_downloaded = n.bytes_downloaded;
			if (c.state !== n.state) c.state = n.state;
		}
	}

	function loadFiles() {
		filesLoading = true;
		filesError = null;
		lastFilesFetch = Date.now();
		fetchQueueJobDetail(slot.nzo_id)
			.then((res) => {
				applyFilesUpdate(res.queue.slots[0]?.files ?? []);
			})
			.catch((e) => {
				filesError = e instanceof Error ? e.message : String(e);
			})
			.finally(() => {
				filesLoading = false;
			});
	}

	/**
	 * Throttle drawer refreshes: at most one fetch per FILES_REFRESH_MS.
	 * Coalesces a burst of queue_updated events (which fire at 1 Hz from
	 * the metrics push) into a single trailing fetch.
	 */
	function scheduleFilesRefresh() {
		if (!expanded) return;
		const since = Date.now() - lastFilesFetch;
		if (since >= FILES_REFRESH_MS) {
			loadFiles();
			return;
		}
		if (pendingFilesRefresh) return;
		pendingFilesRefresh = setTimeout(() => {
			pendingFilesRefresh = null;
			if (expanded) loadFiles();
		}, FILES_REFRESH_MS - since);
	}

	// IMPORTANT: untrack the body so that the parent passing a fresh
	// `slot` prop on every queue.poll() (Svelte 5 props are reactive
	// proxies — even when nzo_id is unchanged, the proxy identity
	// flips) does not trigger this effect's cleanup. Without untrack,
	// every parent poll teared down the drawer's subscription and
	// nulled `files`, briefly flashing "Loading file list…" between
	// the cleanup and the next fetch's resolution.
	$effect(() => {
		if (!expanded) return;
		return untrack(() => {
			loadFiles();
			const unsub = subscribeWS((event) => {
				// Skip refreshes when the tab isn't visible — the
				// drawer reflows on every update, and there's no
				// point paying that cost (or warming the meter on
				// the server) when nobody's looking. We refresh once
				// on visibilitychange when the user returns.
				if (event.event === 'queue_updated' && !document.hidden) {
					scheduleFilesRefresh();
				}
			});
			const onVisibility = () => {
				if (!document.hidden) loadFiles();
			};
			document.addEventListener('visibilitychange', onVisibility);
			return () => {
				unsub();
				document.removeEventListener('visibilitychange', onVisibility);
				if (pendingFilesRefresh) {
					clearTimeout(pendingFilesRefresh);
					pendingFilesRefresh = null;
				}
				files = null;
				filesError = null;
			};
		});
	});

	// Subscribe to postproc_output events while the job is post-processing.
	// Lines accumulate in outputLines; the log auto-scrolls to the bottom.
	$effect(() => {
		if (!isPostProc) {
			outputLines = [];
			return;
		}
		const nzoId = slot.nzo_id;
		const unsub = subscribeWS((event) => {
			if (event.event === 'postproc_output' && event.nzo_id === nzoId && event.line != null) {
				const toolPrefix = event.tool ? `[${event.tool}] ` : '';
				outputLines = [...outputLines.slice(-(MAX_OUTPUT_LINES - 1)), `${toolPrefix}${event.line}`];
				// Auto-scroll after DOM update.
				requestAnimationFrame(() => {
					if (outputLogEl) {
						outputLogEl.scrollTop = outputLogEl.scrollHeight;
					}
				});
			}
		});
		return () => {
			unsub();
		};
	});

	function filePct(f: QueueFile): number {
		if (f.bytes <= 0) return 0;
		return Math.min(100, Math.round((f.bytes_downloaded / f.bytes) * 100));
	}

	function fileStateColor(state: string): string {
		switch (state) {
			case 'done': return 'text-emerald-600 dark:text-emerald-400';
			case 'failed': return 'text-red-600 dark:text-red-400';
			case 'downloading': return 'text-blue-600 dark:text-blue-400';
			default: return 'text-gray-500 dark:text-gray-400';
		}
	}

	async function togglePause() {
		acting = true;
		try {
			if (isPaused) {
				await resumeJob(slot.nzo_id);
			} else {
				await pauseJob(slot.nzo_id);
			}
		} finally {
			acting = false;
		}
	}

	async function changePP(e: Event) {
		const newPP = (e.target as HTMLSelectElement).value;
		try {
			await postAction('queue', { name: 'change_opts', value: slot.nzo_id, value2: newPP });
			slot.pp = newPP;
		} catch (err) {
			console.error('Failed to change PP:', err);
		}
	}

	/** Health indicator: can par2 cover the damage? */
	function healthLabel(): { text: string; color: string } {
		if (slot.failed_bytes === 0) return { text: 'Healthy', color: 'text-emerald-600 dark:text-emerald-400' };
		if (slot.par2_bytes === 0) return { text: 'No repair data', color: 'text-red-600 dark:text-red-400' };
		if (slot.failed_bytes <= slot.par2_bytes) return { text: 'Repairable', color: 'text-amber-600 dark:text-amber-400' };
		return { text: 'Beyond repair', color: 'text-red-600 dark:text-red-400' };
	}
</script>

<tr
	class="border-b hover:bg-gray-50 dark:hover:bg-gray-800 text-gray-900 dark:text-gray-100 cursor-pointer"
	onclick={() => { expanded = !expanded; }}
>
	<td class="px-4 py-3 max-w-[200px] sm:max-w-xs md:max-w-sm lg:max-w-md xl:max-w-lg">
		<div class="flex items-center gap-2">
			<svg
				xmlns="http://www.w3.org/2000/svg"
				viewBox="0 0 16 16"
				fill="currentColor"
				class={cn('size-4 shrink-0 transition-transform', expanded && 'rotate-90')}
			>
				<path
					fill-rule="evenodd"
					d="M6.22 4.22a.75.75 0 0 1 1.06 0l3.25 3.25a.75.75 0 0 1 0 1.06l-3.25 3.25a.75.75 0 0 1-1.06-1.06L8.94 8 6.22 5.28a.75.75 0 0 1 0-1.06Z"
					clip-rule="evenodd"
				/>
			</svg>
			<div class="min-w-0 flex-1">
				<div class="font-medium truncate" title={slot.name || slot.filename}>
					{slot.name || slot.filename}
				</div>
				{#if isDownloading && slot.current_file}
					<div
						class="text-xs text-gray-500 dark:text-gray-400 truncate font-mono"
						title={slot.current_file}
					>
						↓ {slot.current_file}
					</div>
				{:else if isPostProc && outputLines.length > 0}
					<div
						class="text-xs text-gray-500 dark:text-gray-400 truncate font-mono"
						title={outputLines[outputLines.length - 1]}
					>
						⚙ {outputLines[outputLines.length - 1]}
					</div>
				{/if}
			</div>
			{#if slot.warning}
				<div class="flex items-center text-amber-600 shrink-0 max-w-[100px]" title={slot.warning}>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 shrink-0">
						<path
							fill-rule="evenodd"
							d="M6.701 2.25c.577-1 1.419-1 1.998 0l5.156 8.93c.577 1 .158 1.82-1 1.82H3.145c-1.158 0-1.577-.82-1-1.82l5.156-8.93ZM8 5.5a.75.75 0 0 1 .75.75v1.5a.75.75 0 0 1-1.5 0v-1.5A.75.75 0 0 1 8 5.5Zm0 6a.625.625 0 1 0 0-1.25.625.625 0 0 0 0 1.25Z"
							clip-rule="evenodd"
						/>
					</svg>
					<span class="ml-1 text-xs font-semibold truncate">{slot.warning}</span>
				</div>
			{/if}
			{#if hasFailed}
				<span class="shrink-0 text-xs font-medium text-red-500 dark:text-red-400" title="Failed download bytes">
					✗ {formatBytes(slot.failed_bytes)}
				</span>
			{/if}
		</div>
	</td>
	<td class="px-4 py-3">
		<div class="flex w-32 items-center gap-2">
			<Progress
				value={percentage}
				max={100}
				class={cn('h-2 flex-1', isActive && 'animate-pulse')}
			/>
			<span class="text-xs font-mono text-gray-500 dark:text-gray-400 w-9 text-right">{slot.percentage}%</span>
		</div>
	</td>
	<td class="px-4 py-3 text-sm whitespace-nowrap">{slot.size}</td>
	<td class="px-4 py-3 text-sm whitespace-nowrap">
		{slot.sizeleft}
		{#if etaText && isDownloading}
			<span class="ml-1 text-xs text-gray-500 dark:text-gray-400" title="Estimated time remaining">
				· {etaText}
			</span>
		{/if}
	</td>
	<td class="px-4 py-3">
		<Badge variant={isPaused ? 'outline' : isPostProc ? 'secondary' : 'default'} class="text-xs">
			{slot.status}
		</Badge>
	</td>
	<td class="px-4 py-3 text-sm">{slot.cat || '*'}</td>
	<td class="px-4 py-3">
		<div class="flex gap-1">
			<!-- svelte-ignore a11y_click_events_have_key_events -->
			<Button
				variant="ghost"
				size="icon-xs"
				onclick={(e: MouseEvent) => { e.stopPropagation(); togglePause(); }}
				disabled={acting}
				title={isPaused ? 'Resume' : 'Pause'}
			>
				{#if isPaused}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path
							d="M4.5 2A1.5 1.5 0 0 0 3 3.5v9a1.5 1.5 0 0 0 2.3 1.27l7-4.5a1.5 1.5 0 0 0 0-2.54l-7-4.5A1.5 1.5 0 0 0 4.5 2Z"
						/>
					</svg>
				{:else}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3.5">
						<path
							d="M4.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75A.75.75 0 0 0 5.5 2h-1ZM10.5 2a.75.75 0 0 0-.75.75v10.5c0 .414.336.75.75.75h1a.75.75 0 0 0 .75-.75V2.75a.75.75 0 0 0-.75-.75h-1Z"
						/>
					</svg>
				{/if}
			</Button>

			<Button variant="ghost" size="icon-xs" onclick={(e: MouseEvent) => { e.stopPropagation(); onremove(); }} disabled={acting} title="Delete">
				<svg
					xmlns="http://www.w3.org/2000/svg"
					viewBox="0 0 16 16"
					fill="currentColor"
					class="size-3.5 text-red-500"
				>
					<path
						fill-rule="evenodd"
						d="M5 3.25V4H2.75a.75.75 0 0 0 0 1.5h.3l.815 8.15A1.5 1.5 0 0 0 5.357 15h5.285a1.5 1.5 0 0 0 1.493-1.35l.815-8.15h.3a.75.75 0 0 0 0-1.5H11v-.75A2.25 2.25 0 0 0 8.75 1h-1.5A2.25 2.25 0 0 0 5 3.25Zm2.25-.75a.75.75 0 0 0-.75.75V4h3v-.75a.75.75 0 0 0-.75-.75h-1.5ZM6.05 6a.75.75 0 0 1 .787.713l.275 5.5a.75.75 0 0 1-1.498.075l-.275-5.5A.75.75 0 0 1 6.05 6Zm3.9 0a.75.75 0 0 1 .712.787l-.275 5.5a.75.75 0 0 1-1.498-.075l.275-5.5a.75.75 0 0 1 .786-.711Z"
						clip-rule="evenodd"
					/>
				</svg>
			</Button>
		</div>
	</td>
</tr>

{#if expanded}
	<tr class="border-b bg-gray-50/50 dark:bg-gray-800/50">
		<td colspan="7" class="px-6 py-3">
			<div class="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Failed</span>
					{#if slot.failed_bytes > 0}
						<div class="font-medium text-red-600 dark:text-red-400">{formatBytes(slot.failed_bytes)}</div>
					{:else}
						<div class="font-medium text-emerald-600 dark:text-emerald-400">None</div>
					{/if}
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Par2 Recovery</span>
					<div class="font-medium">
						{#if slot.par2_bytes > 0}
							{formatBytes(slot.par2_bytes)}
							<span class="text-gray-400 text-xs">({slot.par2_files} file{slot.par2_files !== 1 ? 's' : ''})</span>
						{:else}
							<span class="text-gray-400">None</span>
						{/if}
					</div>
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Health</span>
					<div class={cn('font-medium', healthLabel().color)}>{healthLabel().text}</div>
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Downloaded</span>
					<div class="font-medium">{formatBytes(slot.bytes - slot.remaining_bytes)} of {slot.size}</div>
				</div>
				<div>
					<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Processing</span>
					<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
					<div
						onclick={(e: MouseEvent) => e.stopPropagation()}
						onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
					>
						<select
							value={slot.pp}
							onchange={changePP}
							class="h-7 rounded border border-gray-300 dark:border-gray-600 bg-transparent px-1.5 text-sm font-medium focus:outline-none focus:ring-1 focus:ring-blue-500 cursor-pointer"
							title="Post-processing level: 0=Download, 1=+Repair, 2=+Unpack, 3=+Delete"
						>
							{#each Object.entries(PP_LABELS) as [val, label] (val)}
								<option value={val}>{label}</option>
							{/each}
						</select>
					</div>
				</div>
				{#if isActive}
					<div>
						<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Articles Left</span>
						<div class="font-medium font-mono">{slot.articles_remaining ?? 0}</div>
					</div>
					{#if etaText}
						<div>
							<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">ETA</span>
							<div class="font-medium font-mono">{etaText}</div>
						</div>
					{/if}
					{#if slot.current_file}
						<div class="col-span-2 md:col-span-4">
							<span class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide">Current File</span>
							<div class="font-medium font-mono text-xs truncate" title={slot.current_file}>{slot.current_file}</div>
						</div>
					{/if}
				{/if}
			</div>

			<!-- Real-time postproc output log: visible while post-processing. -->
			{#if outputLines.length > 0}
				<div class="mt-4">
					<div class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide mb-2">
						Post-Processing Output
						<Badge variant="secondary" class="ml-2 text-[10px] py-0 animate-pulse">Live</Badge>
					</div>
					<div
						bind:this={outputLogEl}
						class="bg-gray-900 text-gray-100 rounded-md p-3 font-mono text-xs leading-relaxed max-h-48 overflow-y-auto scroll-smooth"
					>
						{#each outputLines as line, i (i)}
							<div class="whitespace-pre-wrap break-all">{line}</div>
						{/each}
					</div>
				</div>
			{/if}

			<!-- Per-file breakdown: lazy-fetched while the drawer is open. -->
			<div class="mt-4">
				<div class="text-gray-500 dark:text-gray-400 text-xs uppercase tracking-wide mb-2">
					Files
					{#if files}
						<span class="ml-1 text-gray-400">({files.length})</span>
					{/if}
				</div>
				{#if filesLoading && !files}
					<div class="text-xs text-gray-500 dark:text-gray-400">Loading file list…</div>
				{:else if filesError}
					<div class="text-xs text-red-600 dark:text-red-400" title={filesError}>
						Failed to load file list: {filesError}
					</div>
				{:else if files && files.length > 0}
					<div class="overflow-x-auto">
						<table class="w-full text-xs">
							<thead class="text-gray-500 dark:text-gray-400">
								<tr class="border-b border-gray-200 dark:border-gray-700">
									<th class="text-left py-1 pr-4 font-medium">File</th>
									<th class="text-right py-1 pr-4 font-medium whitespace-nowrap">Size</th>
									<th class="text-right py-1 pr-4 font-medium whitespace-nowrap">Done</th>
									<th class="text-left py-1 pr-4 font-medium w-32">Progress</th>
									<th class="text-left py-1 font-medium">State</th>
								</tr>
							</thead>
							<tbody>
								<!--
									Keyed on array index, not f.name: NZBs commonly contain
									multiple files sharing the same subject (e.g. main +
									sample, or par2 blocks all named after the release).
									Job.Files order is stable across refreshes (NZB source
									order), so position is a valid identity.
								-->
								{#each files as f, i (i)}
									<tr class="border-b border-gray-100 dark:border-gray-800 last:border-0">
										<td class="py-1 pr-4 font-mono truncate max-w-xs" title={f.name}>{f.name}</td>
										<td class="py-1 pr-4 text-right font-mono whitespace-nowrap">{formatBytes(f.bytes)}</td>
										<td class="py-1 pr-4 text-right font-mono whitespace-nowrap">{formatBytes(f.bytes_downloaded)}</td>
										<td class="py-1 pr-4">
											<div class="flex items-center gap-2">
												<Progress value={filePct(f)} max={100} class="h-1.5 flex-1" />
												<span class="font-mono text-gray-500 w-9 text-right">{filePct(f)}%</span>
											</div>
										</td>
										<td class={cn('py-1 capitalize', fileStateColor(f.state))}>{f.state}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else if files}
					<div class="text-xs text-gray-500 dark:text-gray-400">No files in this job.</div>
				{/if}
			</div>
		</td>
	</tr>
{/if}
