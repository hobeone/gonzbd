<script lang="ts">
	let {
		section,
		keyword,
		value,
		label,
		description,
		requiresRestart = false,
		onupdate
	}: {
		section: string;
		keyword: string;
		value: boolean;
		label: string;
		description?: string;
		requiresRestart?: boolean;
		onupdate?: (section: string, keyword: string, value: boolean) => void;
	} = $props();

	function handleChange(e: Event) {
		const checked = (e.target as HTMLInputElement).checked;
		onupdate?.(section, keyword, checked);
	}
</script>

<div class="flex items-center justify-between py-3">
	<div class="space-y-0.5">
		<label for="{section}-{keyword}" class="text-sm font-medium leading-none">
			{label}
			{#if requiresRestart}
				<span class="ml-1.5 inline-flex items-center rounded px-1.5 py-0.5
        text-[10px] font-medium bg-amber-100 text-amber-800
        dark:bg-amber-900/40 dark:text-amber-300">requires restart</span>
			{/if}
		</label>
		{#if description}
			<p class="text-xs text-muted-foreground">
				{description}
			</p>
		{/if}
	</div>
	<div class="flex items-center">
		<input
			id="{section}-{keyword}"
			type="checkbox"
			checked={value}
			onchange={handleChange}
			class="h-4 w-4 rounded border-gray-300 text-blue-600 focus:ring-blue-600"
		/>
	</div>
</div>
