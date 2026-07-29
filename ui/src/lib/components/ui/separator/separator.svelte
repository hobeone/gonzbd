<script lang="ts">
	import { cn } from "$lib/utils.js";

	let {
		ref = $bindable(null),
		class: className,
		orientation = "horizontal",
		"data-slot": dataSlot = "separator",
		...restProps
	}: {
		ref?: HTMLElement | null;
		class?: string;
		orientation?: "horizontal" | "vertical";
		"data-slot"?: string;
		[key: string]: any;
	} = $props();
</script>

<div
	bind:this={ref}
	role="separator"
	aria-orientation={orientation}
	data-slot={dataSlot}
	class={cn(
		"bg-border shrink-0",
		orientation === "horizontal" ? "h-px w-full" : "w-px h-full",
		className
	)}
	{...restProps}
></div>
