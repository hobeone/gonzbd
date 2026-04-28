<script lang="ts">
	import {
		getSpeedBytesPerSec,
		getSpeedHistory,
		getTotalRemainingBytes,
		getSpeedLimitBytesPerSec,
		setSpeedLimit,
		formatSpeed,
		formatSize,
		getQueueSlots,
		isPaused
	} from '$lib/stores/queue.svelte';
	import SpeedGraph from './SpeedGraph.svelte';

	let speed = $derived(getSpeedBytesPerSec());
	let history = $derived(getSpeedHistory());
	let remaining = $derived(getTotalRemainingBytes());
	let paused = $derived(isPaused());
	let itemCount = $derived(getQueueSlots().length);
	let speedLimit = $derived(getSpeedLimitBytesPerSec());

	let eta = $derived.by(() => {
		if (speed <= 0 || remaining <= 0) return '--';
		const seconds = remaining / speed;
		if (seconds < 60) return `${Math.round(seconds)}s`;
		if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
		const h = Math.floor(seconds / 3600);
		const m = Math.round((seconds % 3600) / 60);
		return `${h}h ${m}m`;
	});

	let speedLimitLabel = $derived(speedLimit > 0 ? formatSpeed(speedLimit) : 'Unlimited');

	// Speed limit popover state
	let showPopover = $state(false);
	let customValue = $state('');
	let popoverEl: HTMLDivElement | undefined = $state();

	const presets = [
		{ label: 'Unlimited', kib: 0 },
		{ label: '100 KB/s', kib: 100 },
		{ label: '500 KB/s', kib: 500 },
		{ label: '1 MB/s', kib: 1024 },
		{ label: '5 MB/s', kib: 5120 },
		{ label: '10 MB/s', kib: 10240 },
		{ label: '25 MB/s', kib: 25600 },
		{ label: '50 MB/s', kib: 51200 },
		{ label: '100 MB/s', kib: 102400 }
	];

	function togglePopover() {
		showPopover = !showPopover;
		customValue = '';
	}

	async function applyPreset(kib: number) {
		showPopover = false;
		await setSpeedLimit(kib);
	}

	async function applyCustom() {
		const num = parseFloat(customValue);
		if (isNaN(num) || num < 0) return;
		showPopover = false;
		// Custom value is in MB/s → convert to KiB/s
		await setSpeedLimit(Math.round(num * 1024));
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter') applyCustom();
		if (e.key === 'Escape') showPopover = false;
	}

	// Close on outside click
	function handleWindowClick(e: MouseEvent) {
		if (showPopover && popoverEl && !popoverEl.contains(e.target as Node)) {
			showPopover = false;
		}
	}
</script>

<svelte:window onclick={handleWindowClick} />

<div class="border-t bg-white dark:bg-gray-900">
	<div class="mx-auto flex max-w-7xl items-center gap-4 px-4 py-2 text-sm text-gray-600 dark:text-gray-400">
		<div class="flex items-center gap-2">
			<SpeedGraph data={history} />
			<!-- Clickable speed display -->
			<div class="relative" bind:this={popoverEl}>
				<button
					onclick={togglePopover}
					class="group flex items-center gap-1.5 rounded-md px-2 py-1 transition-colors hover:bg-gray-100 dark:hover:bg-gray-800"
					title="Click to set speed limit"
				>
					<span class="font-mono font-medium text-gray-900 dark:text-gray-100">{formatSpeed(speed)}</span>
					{#if speedLimit > 0}
						<span class="text-xs text-amber-600 dark:text-amber-400">⏬ {speedLimitLabel}</span>
					{/if}
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3 text-gray-400 opacity-0 transition-opacity group-hover:opacity-100">
						<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
					</svg>
				</button>

				{#if showPopover}
					<div
						class="absolute bottom-full left-0 z-50 mb-2 w-56 rounded-lg border border-gray-200 bg-white shadow-lg dark:border-gray-700 dark:bg-gray-800"
						role="menu"
					>
						<div class="border-b border-gray-100 px-3 py-2 dark:border-gray-700">
							<p class="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">Speed Limit</p>
						</div>
						<div class="max-h-52 overflow-y-auto py-1">
							{#each presets as preset}
								<button
									class="flex w-full items-center justify-between px-3 py-1.5 text-left text-sm transition-colors hover:bg-gray-50 dark:hover:bg-gray-700"
									class:text-blue-600={speedLimit === preset.kib * 1024}
									class:dark:text-blue-400={speedLimit === preset.kib * 1024}
									class:font-medium={speedLimit === preset.kib * 1024}
									class:text-gray-700={speedLimit !== preset.kib * 1024}
									class:dark:text-gray-300={speedLimit !== preset.kib * 1024}
									onclick={() => applyPreset(preset.kib)}
									role="menuitem"
								>
									{preset.label}
									{#if speedLimit === preset.kib * 1024}
										<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4">
											<path fill-rule="evenodd" d="M12.416 3.376a.75.75 0 0 1 .208 1.04l-5 7.5a.75.75 0 0 1-1.154.114l-3-3a.75.75 0 0 1 1.06-1.06l2.353 2.353 4.493-6.74a.75.75 0 0 1 1.04-.207Z" clip-rule="evenodd" />
										</svg>
									{/if}
								</button>
							{/each}
						</div>
						<div class="border-t border-gray-100 p-2 dark:border-gray-700">
							<div class="flex items-center gap-1.5">
								<input
									type="text"
									bind:value={customValue}
									onkeydown={handleKeydown}
									placeholder="Custom (MB/s)"
									class="h-7 flex-1 rounded border border-gray-300 bg-transparent px-2 text-xs text-gray-900 placeholder-gray-400 focus:border-blue-500 focus:outline-none dark:border-gray-600 dark:text-gray-100 dark:placeholder-gray-500"
								/>
								<button
									onclick={applyCustom}
									class="h-7 rounded bg-blue-600 px-2.5 text-xs font-medium text-white transition-colors hover:bg-blue-700"
								>
									Set
								</button>
							</div>
						</div>
					</div>
				{/if}
			</div>
		</div>
		<div class="h-4 w-px bg-gray-200 dark:bg-gray-700"></div>
		<span>{itemCount} item{itemCount !== 1 ? 's' : ''}</span>
		<div class="h-4 w-px bg-gray-200 dark:bg-gray-700"></div>
		<span>{formatSize(remaining)} left</span>
		<div class="h-4 w-px bg-gray-200 dark:bg-gray-700"></div>
		<span>ETA: {eta}</span>
		{#if paused}
			<span class="ml-auto font-medium text-yellow-600 dark:text-yellow-400">PAUSED</span>
		{/if}
	</div>
</div>
