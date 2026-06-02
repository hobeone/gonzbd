<script lang="ts">
	import {
		getBandwidthMaxBytesPerSec,
		getBandwidthPerc,
		setBandwidthPerc
	} from '$lib/stores/queue.svelte';

	let bandwidthMax = $derived(getBandwidthMaxBytesPerSec());
	let bandwidthPerc = $derived(getBandwidthPerc());

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

	let maxFormatted = $derived(formatWithUnit(bandwidthMax));
	let effectiveBytes = $derived(bandwidthMax * (isDragging ? sliderValue : bandwidthPerc) / 100);
	let effectiveFormatted = $derived(formatWithUnit(effectiveBytes));
	let displayPerc = $derived(isDragging ? sliderValue : bandwidthPerc);

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

	function togglePopover(e: Event) {
		e.stopPropagation();
		if (!showPopover) {
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

	function handleWindowClick(e: MouseEvent) {
		if (showPopover && popoverEl && !popoverEl.contains(e.target as Node)) {
			showPopover = false;
			isDragging = false;
		}
	}
</script>

<svelte:window onclick={handleWindowClick} />

<div class="relative inline-block" bind:this={popoverEl}>
	<button
		onclick={togglePopover}
		class="group flex items-center gap-1.5 rounded-full bg-muted px-2.5 py-1 text-xs font-semibold text-muted-foreground transition-all hover:bg-muted-foreground/10 hover:text-foreground"
		title="Click to set speed limit"
	>
		<span>Limit:</span>
		<span class="font-bold {bandwidthMax > 0 && bandwidthPerc < 100 ? 'text-amber-500' : 'text-foreground'}">{limitLabel}</span>
		<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-3 text-muted-foreground/60 transition-transform duration-200 group-hover:text-foreground {showPopover ? 'rotate-180' : ''}">
			<path fill-rule="evenodd" d="M4.22 6.22a.75.75 0 0 1 1.06 0L8 8.94l2.72-2.72a.75.75 0 1 1 1.06 1.06l-3.25 3.25a.75.75 0 0 1-1.06 0L4.22 7.28a.75.75 0 0 1 0-1.06Z" clip-rule="evenodd" />
		</svg>
	</button>

	{#if showPopover}
		<div
			class="absolute top-full left-0 z-50 mt-2 w-72 rounded-xl border border-border bg-popover shadow-xl text-popover-foreground p-1 animate-in fade-in slide-in-from-top-1 duration-150"
			role="dialog"
			aria-label="Bandwidth limit"
			tabindex="-1"
			onclick={(e) => e.stopPropagation()}
			onkeydown={(e) => { if (e.key === 'Escape') showPopover = false; }}
		>
			{#if bandwidthMax > 0}
				<!-- Header -->
				<div class="border-b border-border/60 px-4 py-2.5">
					<p class="text-xs font-bold uppercase tracking-wider text-muted-foreground">Bandwidth Limit</p>
					<p class="mt-0.5 text-xs text-muted-foreground">Max: {maxFormatted.value} {maxFormatted.unit}</p>
				</div>

				<!-- Slider -->
				<div class="px-4 py-4">
					<!-- Live value display -->
					<div class="mb-3 flex items-baseline justify-between">
						<span class="text-2xl font-bold tracking-tight tabular-nums text-foreground">{displayPerc}%</span>
						<span class="text-xs font-semibold text-muted-foreground bg-muted px-2 py-0.5 rounded-full">{effectiveFormatted.value} {effectiveFormatted.unit}</span>
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
					<div class="mt-1.5 flex justify-between text-[10px] font-semibold text-muted-foreground/80">
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
					<div class="mx-auto mb-2 flex h-10 w-10 items-center justify-center rounded-full bg-muted">
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="size-5 text-muted-foreground">
							<path fill-rule="evenodd" d="M8.34 1.804A1 1 0 0 1 9.32 1h1.36a1 1 0 0 1 .98.804l.295 1.473c.497.144.971.342 1.416.587l1.25-.834a1 1 0 0 1 1.262.125l.962.962a1 1 0 0 1 .125 1.262l-.834 1.25c.245.445.443.919.587 1.416l1.473.294a1 1 0 0 1 .804.98v1.362a1 1 0 0 1-.804.98l-1.473.295a6.95 6.95 0 0 1-.587 1.416l.834 1.25a1 1 0 0 1-.125 1.262l-.962.962a1 1 0 0 1-1.262.125l-1.25-.834a6.953 6.953 0 0 1-1.416.587l-.294 1.473a1 1 0 0 1-.98.804H9.32a1 1 0 0 1-.98-.804l-.295-1.473a6.957 6.957 0 0 1-1.416-.587l-1.25.834a1 1 0 0 1-1.262-.125l-.962-.962a1 1 0 0 1-.125-1.262l.834-1.25a6.957 6.957 0 0 1-.587-1.416l-1.473-.294A1 1 0 0 1 1 10.68V9.32a1 1 0 0 1 .804-.98l1.473-.295c.144-.497.342-.971.587-1.416l-.834-1.25a1 1 0 0 1 .125-1.262l.962-.962A1 1 0 0 1 5.38 3.03l1.25.834a6.957 6.957 0 0 1 1.416-.587l.294-1.473ZM13 10a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z" clip-rule="evenodd" />
						</svg>
					</div>
					<p class="text-sm font-bold text-foreground">No bandwidth limit set</p>
					<p class="mt-1 text-xs text-muted-foreground">
						Set link speed in Settings → Downloads
					</p>
				</div>
			{/if}
		</div>
	{/if}
</div>

<style>
	/* Custom range slider styling */
	.slider {
		-webkit-appearance: none;
		appearance: none;
		height: 6px;
		border-radius: 3px;
		background: linear-gradient(to right, var(--primary), var(--primary));
		outline: none;
		cursor: pointer;
	}

	.slider::-webkit-slider-thumb {
		-webkit-appearance: none;
		appearance: none;
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: var(--primary);
		border: 2.5px solid white;
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
		width: 18px;
		height: 18px;
		border-radius: 50%;
		background: var(--primary);
		border: 2.5px solid white;
		box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
		cursor: grab;
	}

	.slider::-moz-range-thumb:active {
		cursor: grabbing;
	}

	.slider::-moz-range-track {
		height: 6px;
		border-radius: 3px;
		background: linear-gradient(to right, var(--primary), var(--primary));
	}

	/* Dark mode thumb */
	:global(.dark) .slider::-webkit-slider-thumb {
		border-color: #1f2937;
	}
	:global(.dark) .slider::-moz-range-thumb {
		border-color: #1f2937;
	}
</style>
