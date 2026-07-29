<script lang="ts">
	import { cn } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		class: className,
		max = 100,
		value = 0,
		...restProps
	}: {
		ref?: HTMLElement | null;
		class?: string;
		max?: number;
		value?: number;
		[key: string]: any;
	} = $props();
</script>

<div
	bind:this={ref}
	role="progressbar"
	aria-valuemin={0}
	aria-valuemax={max}
	aria-valuenow={value}
	data-slot="progress"
	class={cn(
		"relative flex h-2 w-full items-center overflow-hidden rounded-full bg-muted",
		className
	)}
	{...restProps}
>
	<div
		data-slot="progress-indicator"
		class="h-full w-full flex-1 bg-primary transition-all"
		style="transform: translateX(-{100 - (100 * (value ?? 0)) / (max ?? 1)}%)"
	></div>
</div>
