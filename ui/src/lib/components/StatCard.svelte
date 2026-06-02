<script lang="ts">
	let {
		title,
		value,
		status = 'idle',
		sparklineData = [],
		children
	}: {
		title: string;
		value: string;
		status?: 'idle' | 'active' | 'error';
		sparklineData?: number[];
		children?: import('svelte').Snippet;
	} = $props();

	// Map data points into SVG polyline coordinates
	const polylinePoints = $derived.by(() => {
		if (sparklineData.length < 2) return '';
		const width = 120;
		const height = 40;
		const maxVal = Math.max(...sparklineData, 1);
		const minVal = Math.min(...sparklineData, 0);
		const valRange = maxVal - minVal;

		return sparklineData
			.map((val, idx) => {
				const x = (idx / (sparklineData.length - 1)) * width;
				const y = height - ((val - minVal) / valRange) * height;
				return `${x},${y}`;
			})
			.join(' ');
	});
</script>

<div class="flex flex-col rounded-xl border border-border bg-card p-5 space-y-4 shadow-sm hover:border-primary/50 transition-colors select-none text-foreground">
	<div class="flex items-center justify-between">
		<span class="text-xs font-semibold uppercase tracking-wider text-muted-foreground">{title}</span>
		<span class="relative flex h-3 w-3">
			<!-- Neon Glow Status Indicator -->
			{#if status === 'active'}
				<span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>
				<span class="relative inline-flex h-3 w-3 rounded-full bg-emerald-500"></span>
			{:else if status === 'error'}
				<span class="absolute inline-flex h-full w-full animate-ping rounded-full bg-rose-400 opacity-75"></span>
				<span class="relative inline-flex h-3 w-3 rounded-full bg-rose-500"></span>
			{:else}
				<span class="relative inline-flex h-3 w-3 rounded-full bg-muted-foreground/50"></span>
			{/if}
		</span>
	</div>

	<div class="flex items-end justify-between gap-4">
		<div class="flex flex-col min-w-0">
			<span class="text-2xl font-bold tracking-tight text-foreground truncate">{value}</span>
			{#if children}
				<div class="mt-1">
					{@render children()}
				</div>
			{/if}
		</div>

		<!-- Ultra-lightweight SVG Sparkline -->
		{#if sparklineData.length >= 2}
			<svg class="h-10 w-28 shrink-0 overflow-visible text-rose-500 dark:text-rose-400" viewBox="0 0 120 40">
				<polyline
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linecap="round"
					stroke-linejoin="round"
					points={polylinePoints}
				/>
			</svg>
		{/if}
	</div>
</div>
