<script lang="ts">
	import { Input } from '$lib/components/ui/input';

	let {
		section,
		keyword,
		value,
		label,
		description,
		placeholder,
		type = 'text',
		onupdate
	}: {
		section: string;
		keyword: string;
		value: string | number;
		label: string;
		description?: string;
		placeholder?: string;
		type?: 'text' | 'number' | 'password';
		onupdate?: (section: string, keyword: string, value: string | number) => void;
	} = $props();

	// Tracks the prop reactively; updates whenever the parent changes it.
	let propValue = $derived(value);

	// Non-null only while the user is actively typing (debounce in progress).
	let editingValue = $state<string | number | null>(null);
	let timer: ReturnType<typeof setTimeout>;

	// Displayed value: local edit override, or the prop.
	let currentValue = $derived(editingValue ?? propValue);

	function commit() {
		clearTimeout(timer);
		const pending = editingValue;
		editingValue = null;
		if (pending != null && pending !== value) {
			const v = type === 'number' ? Number(pending) : pending;
			onupdate?.(section, keyword, v);
		}
	}

	function handleInput(e: Event) {
		const target = e.target as HTMLInputElement;
		editingValue = type === 'number' ? target.value : target.value;
		clearTimeout(timer);
		timer = setTimeout(commit, 500);
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
		value={currentValue}
		{placeholder}
		oninput={handleInput}
		onblur={commit}
		class="max-w-md"
	/>
	{#if description}
		<p class="text-[0.8rem] text-muted-foreground">
			{description}
		</p>
	{/if}
</div>
