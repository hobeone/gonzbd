<script lang="ts">
	import { untrack } from 'svelte';
	import { Textarea } from '$lib/components/ui/textarea';

	let {
		section,
		keyword,
		value = [],
		label,
		description,
		onupdate
	}: {
		section: string;
		keyword: string;
		value: string[];
		label: string;
		description?: string;
		onupdate?: (section: string, keyword: string, value: string) => void;
	} = $props();

	let currentValue = $state<string>((value || []).join('\n'));
	let timer: ReturnType<typeof setTimeout>;

	// When props value (array) changes, update our local string value
	// unless we are currently debouncing a user change.
	$effect(() => {
		const vString = (value || []).join('\n');
		if (vString !== currentValue && !timer) {
			currentValue = vString;
		}
	});

	function commit() {
		clearTimeout(timer);
		const currentArray = currentValue
			.split('\n')
			.map((s) => s.trim())
			.filter((s) => s !== '');
		const currentJson = JSON.stringify(currentArray);
		const propJson = JSON.stringify(value || []);

		if (currentJson !== propJson) {
			onupdate?.(section, keyword, currentJson);
		}
	}

	function handleInput() {
		clearTimeout(timer);
		timer = setTimeout(commit, 1000); // Longer delay for textarea
	}
</script>

<div class="space-y-1.5 py-3">
	<div class="flex items-center justify-between">
		<label for="{section}-{keyword}" class="text-sm font-medium leading-none">
			{label}
		</label>
	</div>
	<Textarea
		id="{section}-{keyword}"
		bind:value={currentValue}
		oninput={handleInput}
		onblur={commit}
		class="max-w-md font-mono text-xs"
		rows={5}
	/>
	{#if description}
		<p class="text-[0.8rem] text-muted-foreground whitespace-pre-line">
			{description}
		</p>
	{/if}
</div>
