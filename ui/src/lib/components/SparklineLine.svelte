<script lang="ts">
	import { getContext } from 'svelte';

	const { data, xGet, yGet } = getContext<any>('LayerCake');

	let { stroke = 'currentColor', strokeWidth = 2, class: className } = $props<{
		stroke?: string;
		strokeWidth?: number;
		class?: string;
	}>();

	let path = $derived.by(() => {
		if (!$data || !$xGet || !$yGet || $data.length < 2) return '';
		return 'M' + $data.map(d => `${$xGet(d)},${$yGet(d)}`).join('L');
	});
</script>

<path
	d={path}
	fill="none"
	{stroke}
	stroke-width={strokeWidth}
	class={className}
	stroke-linecap="round"
	stroke-linejoin="round"
/>
