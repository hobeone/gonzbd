<script lang="ts">
	let { data = [], width = 120, height = 32 }: {
		data?: number[];
		width?: number;
		height?: number;
	} = $props();

	let points = $derived.by(() => {
		if (data.length < 2) return '';
		const max = Math.max(...data, 1);
		const step = width / (data.length - 1);
		return data
			.map((v, i) => `${i * step},${height - (v / max) * (height - 2)}`)
			.join(' ');
	});
</script>

<svg {width} {height} class="overflow-visible">
	{#if data.length >= 2}
		<polyline
			{points}
			fill="none"
			stroke="currentColor"
			stroke-width="1.5"
			stroke-linejoin="round"
			stroke-linecap="round"
			class="text-blue-400"
		/>
	{/if}
</svg>
