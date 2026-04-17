<script lang="ts">
	let {
		section,
		keyword,
		value,
		label,
		description,
		onupdate
	}: {
		section: string;
		keyword: string;
		value: boolean;
		label: string;
		description?: string;
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
