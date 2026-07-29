<script lang="ts">
	import {
		getSpeedBytesPerSec,
		getSpeedHistory,
		getTotalRemainingBytes,
		getSpeedLimitBytesPerSec,
		getBandwidthMaxBytesPerSec,
		getBandwidthPerc,
		setBandwidthPerc,
		getQueueSlots,
		isPaused
	} from '$lib/stores/queue.svelte';
	import { formatSpeed, formatSize } from '$lib/utils';
	import ChevronDown from '@lucide/svelte/icons/chevron-down';
	import Settings from '@lucide/svelte/icons/settings';
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

<div data-testid="status-bar" class="border-b border-border/50 bg-card text-foreground">
	<div class="mx-auto flex max-w-7xl items-center gap-4 px-4 py-2 text-xs font-medium text-muted-foreground">
		<div class="flex items-center gap-2.5">
			<SpeedGraph data={history} />
			<!-- Speed display (read-only) -->
			<span class="font-mono tabular-nums font-semibold text-foreground">{formatSpeed(speed)}</span>
			<!-- Clickable limit label -->
			<div class="relative" bind:this={popoverEl}>
				<button
					onclick={togglePopover}
					class="group flex items-center gap-1.5 rounded-full bg-muted/60 px-2.5 py-1 text-xs font-semibold text-muted-foreground transition-all hover:bg-muted hover:text-foreground"
					title="Click to set speed limit"
				>
					<span class="opacity-75">Limit:</span>
					<span class="font-mono font-bold {bandwidthMax > 0 && bandwidthPerc < 100 ? 'text-amber-500' : 'text-foreground'}">{limitLabel}</span>
					<ChevronDown class="size-3.5 text-muted-foreground transition-transform duration-200 group-hover:translate-y-0.5" />
				</button>

				{#if showPopover}
					<div
						class="absolute top-full left-0 z-50 mt-2 w-72 rounded-2xl border border-border bg-white dark:bg-[#111827] p-4 shadow-2xl text-foreground opacity-100 animate-in fade-in zoom-in-95"
						role="dialog"
						aria-label="Bandwidth limit"
					>
						{#if bandwidthMax > 0}
							<!-- Header -->
							<div class="border-b border-border/60 pb-2.5 mb-2.5">
								<p class="text-[11px] font-bold uppercase tracking-wider text-muted-foreground">Bandwidth Limit</p>
								<p class="mt-0.5 text-xs text-muted-foreground/80 font-mono">Max: {maxFormatted.value} {maxFormatted.unit}</p>
							</div>

							<!-- Slider -->
							<div class="py-1">
								<!-- Live value display -->
								<div class="mb-3 flex items-baseline justify-between">
									<span class="text-2xl font-mono font-bold tabular-nums text-foreground">{displayPerc}%</span>
									<span class="text-xs font-mono font-semibold text-muted-foreground">{effectiveFormatted.value} {effectiveFormatted.unit}</span>
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
									class="slider w-full cursor-pointer"
									aria-label="Bandwidth percentage"
								/>

								<!-- Scale markers -->
								<div class="mt-1 flex justify-between text-[10px] font-mono text-muted-foreground/70">
									<span>1%</span>
									<span>25%</span>
									<span>50%</span>
									<span>75%</span>
									<span>100%</span>
								</div>
							</div>
						{:else}
							<!-- No bandwidth limit configured -->
							<div class="py-4 text-center">
								<div class="mx-auto mb-2 flex h-10 w-10 items-center justify-center rounded-full bg-muted">
									<Settings class="size-5 text-muted-foreground" />
								</div>
								<p class="text-sm font-semibold text-foreground">No bandwidth limit set</p>
								<p class="mt-1 text-xs text-muted-foreground">
									Set a limit in Settings → Downloads
								</p>
							</div>
						{/if}
					</div>
				{/if}
			</div>
		</div>
		<div class="h-3.5 w-px bg-border/60"></div>
		<span class="font-mono tabular-nums">{itemCount} item{itemCount !== 1 ? 's' : ''}</span>
		<div class="h-3.5 w-px bg-border/60"></div>
		<span class="font-mono tabular-nums">{formatSize(remaining)} left</span>
		<div class="h-3.5 w-px bg-border/60"></div>
		<span class="font-mono tabular-nums">ETA: {eta}</span>
		{#if paused}
			<span class="ml-auto font-bold tracking-wider text-[10px] uppercase bg-amber-500/15 text-amber-500 border border-amber-500/30 px-2.5 py-0.5 rounded-full select-none">PAUSED</span>
		{/if}
	</div>
</div>

<style>
	/* Custom range slider styling */
	.slider {
		-webkit-appearance: none;
		appearance: none;
		height: 4px;
		border-radius: 2px;
		background: var(--color-m3-surface-variant);
		outline: none;
		cursor: pointer;
	}

	.slider::-webkit-slider-thumb {
		-webkit-appearance: none;
		appearance: none;
		width: 20px;
		height: 20px;
		border-radius: 50%;
		background: var(--color-m3-primary);
		border: 2px solid var(--color-m3-surface);
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
		cursor: grab;
		transition: transform 0.15s ease, box-shadow 0.15s ease;
	}

	.slider::-webkit-slider-thumb:hover {
		transform: scale(1.15);
		box-shadow: 0 0 0 6px var(--color-m3-primary-container);
	}

	.slider::-webkit-slider-thumb:active {
		cursor: grabbing;
		transform: scale(1.1);
	}

	.slider::-moz-range-thumb {
		width: 20px;
		height: 20px;
		border-radius: 50%;
		background: var(--color-m3-primary);
		border: 2px solid var(--color-m3-surface);
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
		cursor: grab;
	}

	.slider::-moz-range-thumb:active {
		cursor: grabbing;
	}

	.slider::-moz-range-track {
		height: 4px;
		border-radius: 2px;
		background: var(--color-m3-surface-variant);
	}

	/* Dark mode thumb border color */
	:global(.dark) .slider::-webkit-slider-thumb {
		border-color: var(--color-m3-surface);
	}
	:global(.dark) .slider::-moz-range-thumb {
		border-color: var(--color-m3-surface);
	}
</style>
