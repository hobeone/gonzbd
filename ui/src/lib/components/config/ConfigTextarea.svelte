<script lang="ts">
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

	// Tracks the prop reactively as a newline-joined string.
	let propValue = $derived((value || []).join('\n'));

	// Non-null only while the user is actively typing (debounce in progress).
	let editingValue = $state<string | null>(null);
	let timer: ReturnType<typeof setTimeout>;

	// Displayed value: local edit override, or the prop.
	let currentValue = $derived(editingValue ?? propValue);

	function commit() {
		clearTimeout(timer);
		const pending = editingValue;
		editingValue = null;
		if (pending == null) return;

		const currentArray = pending
			.split('\n')
			.map((s) => s.trim())
			.filter((s) => s !== '');
		const currentJson = JSON.stringify(currentArray);
		const propJson = JSON.stringify(value || []);

		if (currentJson !== propJson) {
			onupdate?.(section, keyword, currentJson);
		}
	}

	function handleInput(e: Event) {
		const target = e.target as HTMLTextAreaElement;
		editingValue = target.value;
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
		value={currentValue}
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
