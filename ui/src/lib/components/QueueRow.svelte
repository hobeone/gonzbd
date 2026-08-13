<script lang="ts">
	import type { QueueSlot, QueueFile, RepairState } from '$lib/types';
	import { untrack } from 'svelte';
	import { Progress } from '$lib/components/ui/progress';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import { pauseJob, resumeJob } from '$lib/stores/queue.svelte';
	import { fetchQueueJobDetail, fetchCategories, fetchScripts, postAction } from '$lib/api';
	import { subscribeWS } from '$lib/stores/websocket.svelte';
	import { cn, formatSize as formatBytes, formatETA } from '$lib/utils';
	import ChevronRight from '@lucide/svelte/icons/chevron-right';
	import Edit2 from '@lucide/svelte/icons/edit-2';
	import AlertTriangle from '@lucide/svelte/icons/alert-triangle';
	import Play from '@lucide/svelte/icons/play';
	import Pause from '@lucide/svelte/icons/pause';
	import Trash2 from '@lucide/svelte/icons/trash-2';

	const PP_LABELS: Record<string, string> = {
		'0': 'Download',
		'1': '+Repair',
		'2': '+Unpack',
		'3': '+Delete',
	};

	function ppLabel(pp: string): string {
		return PP_LABELS[pp] ?? `PP${pp}`;
	}

	function formatFailureReason(reason: string | undefined): string {
		if (!reason) return 'Unknown error';
		const lower = reason.toLowerCase();
		if (lower.includes('failed/missing download articles') || lower.includes('incomplete download')) {
			return 'volume had failed or missing download blocks (needs par2 repair)';
		}
		if (lower === 'aborted' || lower.includes('aborted')) {
			return 'extraction was aborted';
		}
		if (lower.includes('password')) {
			return 'incorrect password';
		}
		if (lower.includes('disk full') || lower.includes('no space')) {
			return 'disk is full';
		}
		if (lower.includes('not a rar') || lower.includes('not rar')) {
			return 'not a supported RAR archive';
		}
		let clean = reason;
		if (clean.startsWith('directunpack: ')) {
			clean = clean.substring('directunpack: '.length);
		}
		return clean;
	}

	let { slot, onremove }: { slot: QueueSlot; onremove: () => void } = $props();

	let acting = $state(false);
	let expanded = $state(false);
	let renaming = $state(false);
	let renameValue = $state('');

	// Category selector state: lazily loaded from get_cats on first
	// interaction; shared across collapsed and expanded selectors.
	let categories = $state<string[]>([]);
	let catsLoaded = $state(false);

	// Script selector state: lazily loaded from get_scripts.
	let scripts = $state<string[]>([]);
	let scriptsLoaded = $state(false);

	function ensureCategoriesLoaded() {
		if (catsLoaded) return;
		catsLoaded = true;
		fetchCategories()
			.then((cats) => { categories = cats; })
			.catch((err) => { console.error('Failed to load categories:', err); catsLoaded = false; });
	}

	function ensureScriptsLoaded() {
		if (scriptsLoaded) return;
		scriptsLoaded = true;
		fetchScripts()
			.then((s) => { scripts = s; })
			.catch((err) => { console.error('Failed to load scripts:', err); scriptsLoaded = false; });
	}

	async function changeScript(e: Event) {
		const newScript = (e.target as HTMLSelectElement).value;
		try {
			await postAction('queue', { name: 'change_script', value: slot.nzo_id, value2: newScript });
			slot.script = newScript;
		} catch (err) {
			console.error('Failed to change script:', err);
		}
	}

	async function changeCat(e: Event) {
		const newCat = (e.target as HTMLSelectElement).value;
		try {
			await postAction('queue', { name: 'change_cat', value: slot.nzo_id, value2: newCat });
			slot.cat = newCat;
			// PP is inherited from the new category on the backend;
			// the next queue poll will refresh slot.pp. Force an immediate
			// detail refresh if the drawer is open.
			if (expanded) loadFiles();
		} catch (err) {
			console.error('Failed to change category:', err);
		}
	}

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

	// The durability window, spelled out rather than summed. bytes_pending is
	// what a power loss would cost right now — written, accepted by the OS,
	// not yet covered by an fsync — and adding it to bytes_durable would claim
	// the whole total survives a crash. The two are always shown as two
	// numbers for that reason.
	let durabilityTooltip = $derived.by(() => {
		const parts = [
			`${formatBytes(slot.bytes_durable ?? 0)} durable (fsynced)`,
			`${formatBytes(slot.bytes_pending ?? 0)} written but not yet fsynced`
		];
		parts.push(
			slot.last_barrier_unix
				? `last checkpoint ${new Date(slot.last_barrier_unix * 1000).toLocaleTimeString()}`
				: 'no checkpoint yet'
		);
		return parts.join(' · ');
	});

	// Bytes listed in the drawer that the row's size deliberately excludes.
	// The row reports what the job expects to fetch, which leaves out par2
	// recovery volumes held back ('held') or already ruled unnecessary
	// ('skipped'); the file table lists every file in the NZB at its full
	// declared size. Without stating this the drawer simply totals more than
	// the row above it with nothing to explain the gap (#325).
	let heldBackBytes = $derived(
		(files ?? [])
			.filter((f) => f.state === 'held' || f.state === 'skipped')
			.reduce((sum, f) => sum + f.bytes, 0)
	);

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

	function onVisibility() {
		if (!document.hidden) {
			loadFiles();
		}
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
			return () => {
				unsub();
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

	function fileStateClasses(state: string): string {
		switch (state) {
			case 'done': return 'text-emerald-600 dark:text-emerald-400';
			case 'failed': return 'text-red-600 dark:text-red-400';
			case 'downloading': return 'text-blue-600 dark:text-blue-400';
			case 'held': return 'text-slate-500 dark:text-slate-400';
			case 'skipped': return 'text-slate-500 dark:text-slate-400 italic';
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

	function startRename() {
		renameValue = slot.name || slot.filename;
		renaming = true;
	}

	async function commitRename() {
		const newName = renameValue.trim();
		renaming = false;
		if (!newName || newName === (slot.name || slot.filename)) return;
		try {
			await postAction('queue', { name: 'rename', value: slot.nzo_id, value2: newName });
			slot.name = newName;
		} catch (err) {
			console.error('Failed to rename job:', err);
		}
	}

	function cancelRename() {
		renaming = false;
	}

	/**
	 * Health indicator: copy and colour for the backend's verdict.
	 *
	 * This deliberately holds no arithmetic. The comparison behind
	 * repair_state lives in queue.RepairStateFrom, shared with the two abort
	 * gates; when this function derived it independently from byte figures it
	 * twice reached a red verdict on jobs the downloader was still working
	 * on, and no search of the Go source could find it doing so. Add a case
	 * here, never a condition.
	 */
	const HEALTH_LABELS: Record<RepairState, { text: string; color: string }> = {
		intact: { text: 'Healthy', color: 'text-emerald-600 dark:text-emerald-400' },
		repairable: { text: 'Repairable', color: 'text-amber-600 dark:text-amber-400' },
		unknown: { text: 'Repair data unknown', color: 'text-amber-600 dark:text-amber-400' },
		no_capacity: { text: 'No repair data', color: 'text-red-600 dark:text-red-400' },
		beyond_capacity: { text: 'Beyond repair', color: 'text-red-600 dark:text-red-400' }
	};

	function healthLabel(): { text: string; color: string } {
		// An unrecognized state is a backend the UI has not caught up with.
		// Reporting that beats guessing a verdict for it.
		return HEALTH_LABELS[slot.repair_state] ?? { text: 'Unknown', color: 'text-m3-on-surface-variant' };
	}
</script>

<tr
	class="border-b border-m3-outline/10 hover:bg-m3-surface-variant/25 text-m3-on-surface cursor-pointer transition-colors"
	onclick={() => { expanded = !expanded; }}
>
	<td class="px-5 py-3.5 max-w-[200px] sm:max-w-xs md:max-w-sm lg:max-w-md xl:max-w-lg">
		<div class="flex items-center gap-2">
			<ChevronRight class="size-4 text-muted-foreground transition-transform duration-200 {expanded ? 'rotate-90' : ''} select-none" />
			<div class="min-w-0 flex-1">
				{#if renaming}
					<div
						role="presentation"
						onclick={(e: MouseEvent) => e.stopPropagation()}
						onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
					>
						<!-- svelte-ignore a11y_autofocus -->
						<input
							type="text"
							bind:value={renameValue}
							onkeydown={(e: KeyboardEvent) => { if (e.key === 'Enter') commitRename(); else if (e.key === 'Escape') cancelRename(); }}
							onblur={commitRename}
							class="w-full rounded-lg border border-primary bg-card px-2 py-1 text-sm font-semibold text-foreground focus:outline-none outline-none shadow-sm"
							autofocus
						/>
					</div>
				{:else}
					<div class="flex items-center gap-1.5">
						<div class="font-semibold truncate text-sm text-foreground" title={slot.name || slot.filename}>
							{slot.name || slot.filename}
						</div>
						<button
							onclick={(e: MouseEvent) => { e.stopPropagation(); startRename(); }}
							class="shrink-0 text-muted-foreground/60 hover:text-primary transition-colors flex items-center"
							title="Rename"
						>
							<Edit2 class="size-3.5" />
						</button>
					</div>
				{/if}
				{#if isDownloading && slot.current_file}
					<div
						class="text-xs text-muted-foreground truncate font-mono mt-0.5"
						title={slot.current_file}
					>
						↓ {slot.current_file}
					</div>
				{:else if isPostProc && outputLines.length > 0}
					<div
						class="text-xs text-muted-foreground truncate font-mono mt-0.5"
						title={outputLines[outputLines.length - 1]}
					>
						⚙ {outputLines[outputLines.length - 1]}
					</div>
				{/if}
			</div>
			{#if slot.stall_reason}
				<!-- The stall reason wins over slot.warning rather than being shown
				     beside it: the backend sets both to the same text when it parks a
				     job, and the reason survives the resume a re-evaluation performs
				     while the warning does not. -->
				<div
					class="flex items-center text-destructive shrink-0 max-w-[140px]"
					title={slot.stall_reason}
					data-testid="stall-reason"
				>
					<AlertTriangle class="size-3.5 leading-none shrink-0" />
					<span class="ml-1 text-xs font-bold truncate">{slot.stall_reason}</span>
				</div>
			{:else if slot.warning}
				<div class="flex items-center text-amber-500 shrink-0 max-w-[100px]" title={slot.warning}>
					<AlertTriangle class="size-3.5 leading-none shrink-0" />
					<span class="ml-1 text-xs font-bold truncate">{slot.warning}</span>
				</div>
			{/if}
			{#if hasFailed}
				<span class="shrink-0 text-xs font-bold font-mono text-destructive" title="Failed download bytes">
					✗ {formatBytes(slot.failed_bytes)}
				</span>
			{/if}
			{#if slot.par2_held}
				<Badge variant="outline" class="shrink-0 text-[10px] text-muted-foreground border-border/60" title="Par2 recovery volumes are downloaded only if repair is needed">
					par2 on-demand
				</Badge>
			{/if}
		</div>
	</td>
	<td class="px-5 py-3.5">
		<div class="flex w-32 items-center gap-2">
			<Progress
				value={percentage}
				max={100}
				class={cn('h-1.5 flex-1 bg-muted', isActive && 'animate-pulse')}
			/>
			<span class="text-xs font-mono tabular-nums text-muted-foreground w-9 text-right font-semibold">{slot.percentage}%</span>
		</div>
	</td>
	<td
		class="px-5 py-3.5 text-xs font-mono font-medium whitespace-nowrap tabular-nums"
		title={durabilityTooltip}
		data-testid="durability-tooltip"
	>{slot.size}</td>
	<td class="px-5 py-3.5 text-xs font-mono font-medium whitespace-nowrap tabular-nums">
		{slot.sizeleft}
		{#if etaText && isDownloading}
			<span class="ml-1 text-xs text-muted-foreground font-semibold" title="Estimated time remaining">
				· {etaText}
			</span>
		{/if}
	</td>
	<td class="px-5 py-3.5">
		<Badge variant={isPaused ? 'outline' : isPostProc ? 'secondary' : 'default'} class="text-[11px] font-semibold px-2 py-0.5 rounded-full">
			{slot.status}
		</Badge>
	</td>
	<td class="px-5 py-3.5 text-sm">
		<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
		<div
			onclick={(e: MouseEvent) => e.stopPropagation()}
			onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
		>
			<select
				onchange={changeCat}
				onfocus={ensureCategoriesLoaded}
				class="h-7 rounded-lg border border-border bg-transparent px-1.5 text-xs font-semibold text-foreground focus:outline-none focus:ring-1 focus:ring-primary cursor-pointer max-w-[120px]"
				title="Category (switching inherits PP, script, and priority)"
			>
				{#if categories.length === 0}
					<option selected>{slot.cat || '*'}</option>
				{:else}
					{#each categories as c (c)}
						<option value={c} selected={c === slot.cat || (c === '*' && !slot.cat)}>{c}</option>
					{/each}
				{/if}
			</select>
		</div>
	</td>
	<td class="px-5 py-3.5">
		<div class="flex gap-1 justify-end">
			<!-- svelte-ignore a11y_click_events_have_key_events -->
			<Button
				variant="ghost"
				size="icon-xs"
				onclick={(e: MouseEvent) => { e.stopPropagation(); togglePause(); }}
				disabled={acting}
				class="rounded-full text-muted-foreground hover:bg-muted hover:text-foreground transition-colors"
				title={isPaused ? 'Resume' : 'Pause'}
			>
				{#if isPaused}
					<Play class="size-3.5 fill-current" />
				{:else}
					<Pause class="size-3.5 fill-current" />
				{/if}
			</Button>

			<Button
				variant="ghost"
				size="icon-xs"
				onclick={(e: MouseEvent) => { e.stopPropagation(); onremove(); }}
				disabled={acting}
				class="rounded-full text-destructive hover:bg-destructive/10 transition-colors"
				title="Delete"
			>
				<Trash2 class="size-3.5" />
			</Button>
		</div>
	</td>
</tr>

{#if expanded}
	<tr class="border-b border-m3-outline/10 bg-m3-surface-variant/15 text-m3-on-surface">
		<td colspan="7" class="px-6 py-4">
			<div class="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm">
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Failed</span>
					{#if slot.failed_bytes > 0}
						<div class="font-bold text-red-600 dark:text-red-400 mt-0.5">{formatBytes(slot.failed_bytes)}</div>
					{:else}
						<div class="font-bold text-emerald-600 dark:text-emerald-400 mt-0.5">None</div>
					{/if}
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Par2 Recovery</span>
					<div class="font-bold mt-0.5">
						{#if slot.recovery_bytes > 0}
							{formatBytes(slot.recovery_bytes)}
							<span class="text-m3-on-surface-variant/70 text-xs font-semibold">({slot.recovery_files} file{slot.recovery_files !== 1 ? 's' : ''})</span>
						{:else if slot.repair_state === 'unknown'}
							<!-- A par2 file is present but none matched the volume-naming
							     convention, so its capacity is unmeasured, not absent. -->
							<span class="text-m3-on-surface-variant/60 font-semibold">Unknown</span>
						{:else}
							<span class="text-m3-on-surface-variant/60 font-semibold">None</span>
						{/if}
					</div>
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Health</span>
					<div class={cn('font-bold mt-0.5', healthLabel().color)}>{healthLabel().text}</div>
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Downloaded</span>
					<div class="font-bold mt-0.5">{formatBytes(slot.bytes - slot.remaining_bytes)} of {slot.size}</div>
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Processing</span>
					<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
					<div
						class="mt-1"
						onclick={(e: MouseEvent) => e.stopPropagation()}
						onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
					>
						<select
							onchange={changePP}
							class="h-7 rounded-lg border border-m3-outline bg-transparent px-2 text-xs font-semibold focus:outline-none focus:ring-1 focus:ring-m3-primary cursor-pointer text-m3-on-surface"
							title="Post-processing level: 0=Download, 1=+Repair, 2=+Unpack, 3=+Delete"
						>
							{#each Object.entries(PP_LABELS) as [val, label] (val)}
								<option value={val} selected={val === slot.pp}>{label}</option>
							{/each}
						</select>
					</div>
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Category</span>
					<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
					<div
						class="mt-1"
						onclick={(e: MouseEvent) => e.stopPropagation()}
						onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
					>
						<select
							onchange={changeCat}
							onfocus={ensureCategoriesLoaded}
							class="h-7 rounded-lg border border-m3-outline bg-transparent px-2 text-xs font-semibold focus:outline-none focus:ring-1 focus:ring-m3-primary cursor-pointer text-m3-on-surface"
							title="Category (switching inherits PP, script, and priority)"
						>
							{#if categories.length === 0}
								<option selected>{slot.cat || '*'}</option>
							{:else}
								{#each categories as c (c)}
									<option value={c} selected={c === slot.cat || (c === '*' && !slot.cat)}>{c}</option>
								{/each}
							{/if}
						</select>
					</div>
				</div>
				<div>
					<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Script</span>
					<!-- svelte-ignore a11y_no_static_element_interactions a11y_click_events_have_key_events -->
					<div
						class="mt-1"
						onclick={(e: MouseEvent) => e.stopPropagation()}
						onkeydown={(e: KeyboardEvent) => e.stopPropagation()}
					>
						<select
							onchange={changeScript}
							onfocus={ensureScriptsLoaded}
							class="h-7 rounded-lg border border-m3-outline bg-transparent px-2 text-xs font-semibold focus:outline-none focus:ring-1 focus:ring-m3-primary cursor-pointer text-m3-on-surface"
							title="Post-processing script"
						>
							{#if scripts.length === 0}
								<option selected>{slot.script || 'None'}</option>
							{:else}
								<option value="" selected={!slot.script}>None</option>
								{#each scripts as s (s)}
									<option value={s} selected={s === slot.script}>{s}</option>
								{/each}
							{/if}
						</select>
					</div>
				</div>
				{#if isActive}
					<div>
						<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Articles Left</span>
						<div class="font-bold font-mono mt-0.5">{slot.articles_remaining ?? 0}</div>
					</div>
					{#if etaText}
						<div>
							<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">ETA</span>
							<div class="font-bold font-mono mt-0.5">{etaText}</div>
						</div>
					{/if}
					{#if slot.current_file}
						<div class="col-span-2 md:col-span-4">
							<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Current File</span>
							<div class="font-medium font-mono text-xs truncate mt-0.5 text-m3-on-surface-variant" title={slot.current_file}>{slot.current_file}</div>
						</div>
					{/if}
				{/if}
				{#if slot.direct_unpack}
					<div>
						<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Direct Unpack</span>
						<div class="font-bold mt-0.5">
							{#if slot.direct_unpack.active}
								<span class="text-amber-600 dark:text-amber-400">Extracting: {slot.direct_unpack.current_set}</span>
								{#if slot.direct_unpack.completed_volumes !== undefined && slot.direct_unpack.total_volumes}
									<span class="text-xs text-m3-on-surface-variant/70 font-mono ml-1">
										({slot.direct_unpack.completed_volumes}/{slot.direct_unpack.total_volumes} vols)
									</span>
								{/if}
							{:else if slot.direct_unpack.success_sets?.length || slot.direct_unpack.failed_sets?.length}
								{#if slot.direct_unpack.failed_sets?.length}
									<span class="text-red-600 dark:text-red-400">
										Unpack failed: {formatFailureReason(slot.direct_unpack.failed_reasons?.[slot.direct_unpack.failed_sets[0]])}
									</span>
									<span class="text-xs text-m3-on-surface-variant/70 font-mono ml-1">
										({slot.direct_unpack.success_sets?.length ?? 0} OK, {slot.direct_unpack.failed_sets.length} Failed)
									</span>
								{:else}
									<span class="text-emerald-600 dark:text-emerald-400">
										Finished ({slot.direct_unpack.success_sets?.length ?? 0} OK)
									</span>
								{/if}
							{:else}
								<span class="text-m3-on-surface-variant/60 font-semibold">Idle</span>
							{/if}
						</div>
					</div>
				{/if}
				{#if slot.direct_unpack?.active && slot.direct_unpack.completed_volumes !== undefined && slot.direct_unpack.total_volumes}
					<div class="col-span-2 md:col-span-4">
						<span class="text-m3-primary text-[10px] font-bold uppercase tracking-wider">Direct Unpack Progress</span>
						<div class="flex items-center gap-2 mt-1">
							<Progress
								value={slot.direct_unpack.completed_volumes}
								max={slot.direct_unpack.total_volumes}
								class="h-1.5 flex-1 bg-m3-surface-variant/40"
							/>
							<span class="text-xs font-mono text-m3-on-surface-variant/80 w-12 text-right">
								{slot.direct_unpack.total_volumes > 0 ? Math.round((slot.direct_unpack.completed_volumes / slot.direct_unpack.total_volumes) * 100) : 0}%
							</span>
						</div>
					</div>
				{/if}
			</div>

			<!-- Real-time postproc output log: visible while post-processing. -->
			{#if outputLines.length > 0}
				<div class="mt-4">
					<div class="text-m3-primary text-[10px] font-bold uppercase tracking-wider mb-2 flex items-center">
						Post-Processing Output
						<Badge variant="secondary" class="ml-2 text-[10px] py-0 px-2 rounded-full animate-pulse bg-emerald-500/10 text-emerald-500 border border-emerald-500/20">Live</Badge>
					</div>
					<div
						bind:this={outputLogEl}
						class="bg-gray-950 text-emerald-400 rounded-2xl border border-m3-outline/10 p-4 font-mono text-xs leading-relaxed max-h-48 overflow-y-auto scroll-smooth"
					>
						{#each outputLines as line, i (i)}
							<div class="whitespace-pre-wrap break-all">{line}</div>
						{/each}
					</div>
				</div>
			{/if}

			<!-- Per-file breakdown: lazy-fetched while the drawer is open. -->
			<div class="mt-4">
				<div class="text-m3-primary text-[10px] font-bold uppercase tracking-wider mb-2">
					Files
					{#if files}
						<span class="ml-1 text-m3-on-surface-variant/80">({files.length})</span>
					{/if}
					{#if heldBackBytes > 0}
						<span
							class="ml-2 normal-case tracking-normal font-semibold text-slate-500 dark:text-slate-400"
							title="Par2 recovery volumes are listed here but excluded from the job's size above, because the job does not expect to download them."
							>incl. {formatBytes(heldBackBytes)} held back</span
						>
					{/if}
				</div>
				{#if filesLoading && !files}
					<div class="text-xs text-m3-on-surface-variant/70 font-semibold">Loading file list…</div>
				{:else if filesError}
					<div class="text-xs text-destructive font-semibold" title={filesError}>
						Failed to load file list: {filesError}
					</div>
				{:else if files && files.length > 0}
					<div class="overflow-x-auto rounded-2xl border border-m3-outline/25 bg-m3-surface/30 px-4 py-2">
						<table class="w-full text-xs">
							<thead class="text-m3-primary border-b border-m3-outline/10">
								<tr>
									<th class="text-left py-2 pr-4 text-[10px] font-bold uppercase tracking-wider">File</th>
									<th class="text-right py-2 pr-4 text-[10px] font-bold uppercase tracking-wider whitespace-nowrap">Size</th>
									<th class="text-right py-2 pr-4 text-[10px] font-bold uppercase tracking-wider whitespace-nowrap">Done</th>
									<th class="text-left py-2 pr-4 text-[10px] font-bold uppercase tracking-wider w-32">Progress</th>
									<th class="text-left py-2 text-[10px] font-bold uppercase tracking-wider">State</th>
								</tr>
							</thead>
							<tbody>
								{#each files as f, i (i)}
									<tr class="border-b border-m3-outline/5 last:border-0 hover:bg-m3-surface-variant/10">
										<td class="py-1.5 pr-4 font-mono truncate max-w-xs text-m3-on-surface" title={f.name}>{f.name}</td>
										<td class="py-1.5 pr-4 text-right font-mono whitespace-nowrap text-m3-on-surface font-medium">{formatBytes(f.bytes)}</td>
										<td class="py-1.5 pr-4 text-right font-mono whitespace-nowrap text-m3-on-surface font-medium">{formatBytes(f.bytes_downloaded)}</td>
										<td class="py-1.5 pr-4">
											<div class="flex items-center gap-2">
												<Progress value={filePct(f)} max={100} class="h-1.5 flex-1 bg-m3-surface-variant/40" />
												<span class="font-mono text-m3-on-surface-variant/80 w-9 text-right font-semibold">{filePct(f)}%</span>
											</div>
										</td>
										<td class={cn('py-1.5 capitalize font-semibold text-xs', fileStateClasses(f.state))}>{f.state}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{:else if files}
					<div class="text-xs text-m3-on-surface-variant/70 font-semibold">No files in this job.</div>
				{/if}
			</div>
		</td>
	</tr>
{/if}

<svelte:document onvisibilitychange={onVisibility} />
