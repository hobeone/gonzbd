<script lang="ts">
	import {
		getSpeedBytesPerSec,
		getSpeedHistory,
		getTotalRemainingBytes,
		getSpeedLimitBytesPerSec,
		getBandwidthMaxBytesPerSec,
		getBandwidthPerc,
		setBandwidthPerc,
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
	let bandwidthMax = $derived(getBandwidthMaxBytesPerSec());
	let bandwidthPerc = $derived(getBandwidthPerc());

	let eta = $derived.by(() => {
		if (speed <= 0 || remaining <= 0) return '--';
		const seconds = remaining / speed;
		if (seconds < 60) return `${Math.round(seconds)}s`;
		if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
		const h = Math.floor(seconds / 3600);
		const m = Math.round((seconds % 3600) / 60);
		return `${h}h ${m}m`;
	});

	// Slider popover state
	let showPopover = $state(false);
	let popoverEl: HTMLDivElement | undefined = $state();
	let sliderValue = $state(100);
	let isDragging = $state(false);

	// Pick the best unit for the max bandwidth
	function formatWithUnit(bytes: number): { value: string; unit: string } {
		if (bytes >= 1024 * 1024 * 1024) return { value: (bytes / (1024 * 1024 * 1024)).toFixed(1), unit: 'GB/s' };
		if (bytes >= 1024 * 1024) return { value: (bytes / (1024 * 1024)).toFixed(1), unit: 'MB/s' };
		if (bytes >= 1024) return { value: (bytes / 1024).toFixed(1), unit: 'KB/s' };
		return { value: bytes.toFixed(0), unit: 'B/s' };
	}

	// The max bandwidth formatted in its natural unit
	let maxFormatted = $derived(formatWithUnit(bandwidthMax));

	// The effective speed at the current slider position
	let effectiveBytes = $derived(bandwidthMax * (isDragging ? sliderValue : bandwidthPerc) / 100);
	let effectiveFormatted = $derived(formatWithUnit(effectiveBytes));
	let displayPerc = $derived(isDragging ? sliderValue : bandwidthPerc);

	// Compact limit label: "10M", "500K", "1G", or "Unlim"
	function formatCompact(bytes: number): string {
		if (bytes <= 0) return 'Unlim';
		if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(bytes % (1024 * 1024 * 1024) === 0 ? 0 : 1)}G`;
		if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(bytes % (1024 * 1024) === 0 ? 0 : 1)}M`;
		if (bytes >= 1024) return `${(bytes / 1024).toFixed(bytes % 1024 === 0 ? 0 : 1)}K`;
		return `${bytes}B`;
	}

	let limitLabel = $derived(
		bandwidthMax > 0 && bandwidthPerc < 100
			? formatCompact(bandwidthMax * bandwidthPerc / 100)
			: formatCompact(bandwidthMax)
	);

	function togglePopover() {
		if (!showPopover) {
			// Initialize slider to current percentage
			sliderValue = bandwidthPerc;
		}
		showPopover = !showPopover;
	}

	function onSliderInput(e: Event) {
		isDragging = true;
		sliderValue = parseInt((e.target as HTMLInputElement).value);
	}

	async function onSliderChange(e: Event) {
		const value = parseInt((e.target as HTMLInputElement).value);
		isDragging = false;
		sliderValue = value;
		await setBandwidthPerc(value);
	}

	// Close on outside click
	function handleWindowClick(e: MouseEvent) {
		if (showPopover && popoverEl && !popoverEl.contains(e.target as Node)) {
			showPopover = false;
			isDragging = false;
		}
	}
</script>

<svelte:window onclick={handleWindowClick} />

<div class="border-t bg-white dark:bg-gray-900">
	<div class="mx-auto flex max-w-7xl items-center gap-4 px-4 py-2 text-sm text-gray-600 dark:text-gray-400">
		<div class="flex items-center gap-2">
			<SpeedGraph data={history} />
			<!-- Speed display (read-only) -->
			<span class="font-mono font-medium text-gray-900 dark:text-gray-100">{formatSpeed(speed)}</span>
			<!-- Clickable limit label -->
			<div class="relative" bind:this={popoverEl}>
				<button
					onclick={togglePopover}
					class="group flex items-center gap-1 rounded-md px-1.5 py-0.5 text-xs transition-colors hover:bg-gray-100 dark:hover:bg-gray-800"
					title="Click to set speed limit"
				>
					<span class="text-gray-500 dark:text-gray-400">Limit:</span>
					<span class="font-medium" class:text-amber-600={bandwidthMax > 0 && bandwidthPerc < 100} class:dark:text-amber-400={bandwidthMax > 0 && bandwidthPerc < 100} class:text-gray-700={!(bandwidthMax > 0 && bandwidthPerc < 100)} class:dark:text-gray-300={!(bandwidthMax > 0 && bandwidthPerc < 100)}>{limitLabel}</span>
					<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3 text-gray-400 opacity-0 transition-opacity group-hover:opacity-100">
						<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
					</svg>
				</button>

				{#if showPopover}
					<div
						class="absolute top-full left-0 z-50 mt-2 w-72 rounded-lg border border-gray-200 bg-white shadow-lg dark:border-gray-700 dark:bg-gray-800"
						role="dialog"
						aria-label="Bandwidth limit"
					>
						{#if bandwidthMax > 0}
							<!-- Header -->
							<div class="border-b border-gray-100 px-4 py-2.5 dark:border-gray-700">
								<p class="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-gray-400">Bandwidth Limit</p>
								<p class="mt-0.5 text-xs text-gray-400 dark:text-gray-500">Max: {maxFormatted.value} {maxFormatted.unit}</p>
							</div>

							<!-- Slider -->
							<div class="px-4 py-4">
								<!-- Live value display -->
								<div class="mb-3 flex items-baseline justify-between">
									<span class="text-2xl font-bold tabular-nums text-gray-900 dark:text-gray-100">{displayPerc}%</span>
									<span class="text-sm font-medium text-gray-500 dark:text-gray-400">{effectiveFormatted.value} {effectiveFormatted.unit}</span>
								</div>

								<!-- Range input -->
								<input
									type="range"
									min="1"
									max="100"
									step="1"
									value={isDragging ? sliderValue : bandwidthPerc}
									oninput={onSliderInput}
									onchange={onSliderChange}
									class="slider w-full"
									aria-label="Bandwidth percentage"
								/>

								<!-- Scale markers -->
								<div class="mt-1 flex justify-between text-[10px] text-gray-400 dark:text-gray-500">
									<span>1%</span>
									<span>25%</span>
									<span>50%</span>
									<span>75%</span>
									<span>100%</span>
								</div>
							</div>
						{:else}
							<!-- No bandwidth limit configured -->
							<div class="px-4 py-5 text-center">
								<div class="mx-auto mb-2 flex h-10 w-10 items-center justify-center rounded-full bg-gray-100 dark:bg-gray-700">
									<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5 text-gray-400">
										<path fill-rule="evenodd" d="M8.34 1.804A1 1 0 0 1 9.32 1h1.36a1 1 0 0 1 .98.804l.295 1.473c.497.144.971.342 1.416.587l1.25-.834a1 1 0 0 1 1.262.125l.962.962a1 1 0 0 1 .125 1.262l-.834 1.25c.245.445.443.919.587 1.416l1.473.294a1 1 0 0 1 .804.98v1.362a1 1 0 0 1-.804.98l-1.473.295a6.95 6.95 0 0 1-.587 1.416l.834 1.25a1 1 0 0 1-.125 1.262l-.962.962a1 1 0 0 1-1.262.125l-1.25-.834a6.953 6.953 0 0 1-1.416.587l-.294 1.473a1 1 0 0 1-.98.804H9.32a1 1 0 0 1-.98-.804l-.295-1.473a6.957 6.957 0 0 1-1.416-.587l-1.25.834a1 1 0 0 1-1.262-.125l-.962-.962a1 1 0 0 1-.125-1.262l.834-1.25a6.957 6.957 0 0 1-.587-1.416l-1.473-.294A1 1 0 0 1 1 10.68V9.32a1 1 0 0 1 .804-.98l1.473-.295c.144-.497.342-.971.587-1.416l-.834-1.25a1 1 0 0 1 .125-1.262l.962-.962A1 1 0 0 1 5.38 3.03l1.25.834a6.957 6.957 0 0 1 1.416-.587l.294-1.473ZM13 10a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z" clip-rule="evenodd" />
									</svg>
								</div>
								<p class="text-sm font-medium text-gray-700 dark:text-gray-300">No bandwidth limit set</p>
								<p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
									Set a limit in Settings → Downloads
								</p>
							</div>
						{/if}
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

<style>
	/* Custom range slider styling */
	.slider {
		-webkit-appearance: none;
		appearance: none;
		height: 6px;
		border-radius: 3px;
		background: linear-gradient(to right, #3b82f6, #60a5fa);
		outline: none;
		cursor: pointer;
	}

	.slider::-webkit-slider-thumb {
		-webkit-appearance: none;
		appearance: none;
		width: 20px;
		height: 20px;
		border-radius: 50%;
		background: #3b82f6;
		border: 3px solid white;
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
		cursor: grab;
		transition: transform 0.15s ease, box-shadow 0.15s ease;
	}

	.slider::-webkit-slider-thumb:hover {
		transform: scale(1.15);
		box-shadow: 0 2px 6px rgba(59, 130, 246, 0.4);
	}

	.slider::-webkit-slider-thumb:active {
		cursor: grabbing;
		transform: scale(1.1);
		box-shadow: 0 0 0 4px rgba(59, 130, 246, 0.2);
	}

	.slider::-moz-range-thumb {
		width: 20px;
		height: 20px;
		border-radius: 50%;
		background: #3b82f6;
		border: 3px solid white;
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
		cursor: grab;
	}

	.slider::-moz-range-thumb:active {
		cursor: grabbing;
	}

	.slider::-moz-range-track {
		height: 6px;
		border-radius: 3px;
		background: linear-gradient(to right, #3b82f6, #60a5fa);
	}

	/* Dark mode thumb */
	:global(.dark) .slider::-webkit-slider-thumb {
		border-color: #1f2937;
	}
	:global(.dark) .slider::-moz-range-thumb {
		border-color: #1f2937;
	}
</style>
