<script lang="ts">
	import { Input } from '$lib/components/ui/input';

	let {
		section,
		keyword,
		value,
		label,
		description,
		type = 'text',
		onupdate
	}: {
		section: string;
		keyword: string;
		value: string | number;
		label: string;
		description?: string;
		type?: 'text' | 'number' | 'password';
		onupdate?: (section: string, keyword: string, value: string | number) => void;
	} = $props();

	let currentValue = $state<string | number>('');
	let timer: ReturnType<typeof setTimeout>;

	$effect(() => {
		if (value !== currentValue && !timer) {
			currentValue = value;
		}
	});

	function handleInput() {
		clearTimeout(timer);
		timer = setTimeout(() => {
			if (currentValue !== value) {
				const v = type === 'number' ? Number(currentValue) : currentValue;
				onupdate?.(section, keyword, v);
			}
		}, 500);
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
