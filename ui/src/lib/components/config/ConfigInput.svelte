<script lang="ts">
	import { Input } from '$lib/components/ui/input';
	import { updateField } from '$lib/stores/config.svelte';

	let {
		section,
		keyword,
		value,
		label,
		description,
		type = 'text'
	}: {
		section: string;
		keyword: string;
		value: string | number;
		label: string;
		description?: string;
		type?: 'text' | 'number' | 'password';
	} = $props();

	let currentValue = $state(value);
	let timer: ReturnType<typeof setTimeout>;

	// When the prop 'value' changes from above (e.g. on load or revert), 
	// update our local draft value.
	$effect(() => {
		if (value !== currentValue && !timer) {
			currentValue = value;
		}
	});

	function handleInput() {
		clearTimeout(timer);
		timer = setTimeout(() => {
			if (currentValue !== value) {
				updateField(section, keyword, type === 'number' ? Number(currentValue) : currentValue);
			}
		}, 500); // 500ms debounce
	}
</script>

<div class="space-y-1.5 py-3">
	<div class="flex items-center justify-between">
		<label for="{section}-{keyword}" class="text-sm font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70">
			{label}
		</label>
	</div>
	<Input
		id="{section}-{keyword}"
		{type}
		bind:value={currentValue}
		oninput={handleInput}
		class="max-w-md"
	/>
	{#if description}
		<p class="text-[0.8rem] text-muted-foreground">
			{description}
		</p>
	{/if}
</div>
