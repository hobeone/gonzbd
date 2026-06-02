<script lang="ts">
	import { getContext } from 'svelte';

	const { data, xGet, yGet, height } = getContext<any>('LayerCake');

	let { fill = 'currentColor', opacity = 0.1, class: className } = $props<{
		fill?: string;
		opacity?: number;
		class?: string;
	}>();

	let path = $derived.by(() => {
		if (!$data || !$xGet || !$yGet || $data.length < 2) return '';
		const points = $data.map(d => `${$xGet(d)},${$yGet(d)}`);
		const firstX = $xGet($data[0]);
		const lastX = $xGet($data[$data.length - 1]);
		return `M${firstX},${$height}L` + points.join('L') + `L${lastX},${$height}Z`;
	});
</script>

<path
	d={path}
	{fill}
	{opacity}
	class={className}
/>
