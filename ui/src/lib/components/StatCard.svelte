<script lang="ts">
	import { LayerCake, Svg } from 'layercake';
	import SparklineLine from './SparklineLine.svelte';
	import SparklineArea from './SparklineArea.svelte';

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

	const chartData = $derived(
		sparklineData.map((y, x) => ({ x, y }))
	);
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

		<!-- LayerCake-based Sparkline -->
		{#if sparklineData.length >= 2}
			<div class="h-10 w-28 shrink-0 relative overflow-visible">
				<LayerCake
					x="x"
					y="y"
					data={chartData}
					yDomain={[0, null]}
				>
					<Svg>
						<SparklineArea class="text-rose-500/10 dark:text-rose-400/10" />
						<SparklineLine class="text-rose-500 dark:text-rose-400" strokeWidth={2} />
					</Svg>
				</LayerCake>
			</div>
		{/if}
	</div>
</div>
