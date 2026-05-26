<script lang="ts">
	import { cn } from "$lib/utils.js";

	let {
		section,
		keyword,
		value,
		label,
		description,
		options,
		requiresRestart = false,
		onupdate
	}: {
		section: string;
		keyword: string;
		value: string;
		label: string;
		description?: string;
		options: { value: string; label: string }[];
		requiresRestart?: boolean;
		onupdate?: (section: string, keyword: string, value: string) => void;
	} = $props();

	function handleChange(e: Event) {
		const target = e.target as HTMLSelectElement;
		onupdate?.(section, keyword, target.value);
	}
</script>

<div class="space-y-1.5 py-3">
	<div class="flex items-center justify-between">
		<label for="{section}-{keyword}" class="text-sm font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70">
			{label}
			{#if requiresRestart}
				<span class="ml-1.5 inline-flex items-center rounded px-1.5 py-0.5
        text-[10px] font-medium bg-amber-100 text-amber-800
        dark:bg-amber-900/40 dark:text-amber-300">requires restart</span>
			{/if}
		</label>
	</div>
	<select
		id="{section}-{keyword}"
		value={value}
		onchange={handleChange}
		class={cn(
			"dark:bg-input/30 border-input focus-visible:border-ring focus-visible:ring-ring/50 h-8 rounded-lg border bg-transparent px-2.5 py-1 text-base transition-colors focus-visible:ring-3 md:text-sm w-full max-w-md outline-none disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 cursor-pointer"
		)}
	>
		{#each options as opt}
			<option value={opt.value}>{opt.label}</option>
		{/each}
	</select>
	{#if description}
		<p class="text-[0.8rem] text-muted-foreground">
			{description}
		</p>
	{/if}
</div>
