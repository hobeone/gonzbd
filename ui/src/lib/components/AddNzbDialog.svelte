<script lang="ts">
	import Modal from '$lib/components/ui/Modal.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { uploadNzb, postAction, fetchCategories } from '$lib/api';
	import { refreshQueue } from '$lib/stores/queue.svelte';
	import FileUp from '@lucide/svelte/icons/file-up';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let activeTab = $state<'file' | 'url'>('file');
	let url = $state('');
	let category = $state('*');
	let password = $state('');
	let priority = $state('-100');
	let paused = $state(false);
	let categories = $state.raw<string[]>(['*']);
	let files = $state<FileList | null>(null);
	let dragging = $state(false);
	let submitting = $state(false);
	let result = $state<{ ok: boolean; message: string } | null>(null);

	$effect(() => {
		if (open) {
			reset();
			fetchCategories().then((cats) => {
				const filtered = cats.filter(c => c !== '*');
				categories = ['*', ...filtered];
			});
		}
	});

	function reset() {
		url = '';
		files = null;
		category = '*';
		password = '';
		priority = '-100';
		paused = false;
		dragging = false;
		submitting = false;
		result = null;
	}

	function handlePriorityChange(e: Event) {
		const val = (e.target as HTMLSelectElement).value;
		priority = val;
		if (val === '-2') {
			paused = true;
		} else if (paused) {
			paused = false;
		}
	}

	function handlePausedChange(e: Event) {
		const checked = (e.target as HTMLInputElement).checked;
		paused = checked;
		if (checked) {
			priority = '-2';
		} else if (priority === '-2') {
			priority = '-100';
		}
	}

	async function submitFile() {
		if (!files || files.length === 0) return;
		submitting = true;
		result = null;
		try {
			const params: Record<string, string> = {};
			if (category !== '*') params.cat = category;
			if (password) params.password = password;
			const finalPriority = paused ? '-2' : priority;
			if (finalPriority !== '-100') params.priority = finalPriority;

			await uploadNzb(files[0], params);
			open = false;
			refreshQueue();
		} catch (e) {
			result = { ok: false, message: e instanceof Error ? e.message : String(e) };
		} finally {
			submitting = false;
		}
	}

	async function submitUrl() {
		const trimmed = url.trim();
		if (!trimmed) return;
		submitting = true;
		result = null;
		try {
			const params: Record<string, string> = { name: trimmed };
			if (category !== '*') params.cat = category;
			if (password) params.password = password;
			const finalPriority = paused ? '-2' : priority;
			if (finalPriority !== '-100') params.priority = finalPriority;

			await postAction('addurl', params);
			open = false;
			refreshQueue();
		} catch (e) {
			result = { ok: false, message: e instanceof Error ? e.message : String(e) };
		} finally {
			submitting = false;
		}
	}

	function handleDrop(e: DragEvent) {
		e.preventDefault();
		dragging = false;
		if (e.dataTransfer?.files.length) {
			files = e.dataTransfer.files;
			activeTab = 'file';
		}
	}

	function handleDragOver(e: DragEvent) {
		e.preventDefault();
		dragging = true;
	}
</script>

<Modal bind:open ariaLabel="Add NZB" class="w-full max-w-md p-6">
	<div
		role="region"
		aria-label="NZB Upload Target"
		ondrop={handleDrop}
		ondragover={handleDragOver}
		ondragleave={() => (dragging = false)}
	>
		<h2 class="text-lg font-semibold tracking-tight text-foreground">Add NZB</h2>
		<p class="mt-1 text-sm text-muted-foreground">
			Upload an NZB file or paste a URL.
		</p>

		<div class="mt-4">
			<div class="flex gap-1 border-b border-border/60">
				<button
					type="button"
					onclick={() => (activeTab = 'file')}
					class="border-b-2 px-4 py-2 text-sm font-semibold tracking-wide transition-all {activeTab === 'file' ? 'border-primary text-primary' : 'border-transparent text-muted-foreground'}"
				>
					File
				</button>
				<button
					type="button"
					onclick={() => (activeTab = 'url')}
					class="border-b-2 px-4 py-2 text-sm font-semibold tracking-wide transition-all {activeTab === 'url' ? 'border-primary text-primary' : 'border-transparent text-muted-foreground'}"
				>
					URL
				</button>
			</div>

			<div class="mt-4 grid grid-cols-2 gap-4">
				<div class="space-y-1.5">
					<label for="category" class="text-xs font-semibold text-muted-foreground">Category</label>
					<select
						id="category"
						bind:value={category}
						class="flex h-9 w-full rounded-lg border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary disabled:cursor-not-allowed disabled:opacity-50"
					>
						{#each categories as cat (cat)}
							<option value={cat}>{cat}</option>
						{/each}
					</select>
				</div>
				<div class="space-y-1.5">
					<label for="priority" class="text-xs font-semibold text-muted-foreground">Priority</label>
					<select
						id="priority"
						value={priority}
						onchange={handlePriorityChange}
						class="flex h-9 w-full rounded-lg border border-input bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary disabled:cursor-not-allowed disabled:opacity-50"
					>
						<option value="-100">Default</option>
						<option value="2">Force</option>
						<option value="1">High</option>
						<option value="0">Normal</option>
						<option value="-1">Low</option>
						<option value="-2">Paused</option>
					</select>
				</div>
			</div>

			<div class="mt-4 grid grid-cols-2 gap-4 items-center">
				<div class="space-y-1.5">
					<label for="password" class="text-xs font-semibold text-muted-foreground">Password</label>
					<Input
						id="password"
						type="text"
						placeholder="Optional"
						bind:value={password}
						class="h-9"
					/>
				</div>
				<div class="pt-5">
					<label class="flex items-center gap-2 cursor-pointer text-sm font-medium text-foreground select-none">
						<input
							type="checkbox"
							id="paused"
							checked={paused}
							onchange={handlePausedChange}
							class="size-4 rounded border-input bg-transparent text-primary focus:ring-primary"
						/>
						<span>Add Paused</span>
					</label>
				</div>
			</div>

			{#if activeTab === 'file'}
				<div class="mt-4">
					<label
						class="flex cursor-pointer flex-col items-center justify-center rounded-3xl border-2 border-dashed p-8 transition-colors
						{dragging ? 'border-primary bg-primary/10' : 'border-border/60 hover:border-primary/70 bg-transparent'}"
					>
						<FileUp class="size-10 text-muted-foreground/80 mb-2" />
						{#if files && files.length > 0}
							<span class="block w-full max-w-[200px] sm:max-w-xs text-sm font-medium text-foreground truncate text-center" title={files[0].name}>{files[0].name}</span>
							<span class="mt-1 text-xs text-muted-foreground">{(files[0].size / 1024).toFixed(1)} KB</span>
						{:else}
							<span class="text-sm text-muted-foreground text-center font-medium">Drop NZB file here or click to browse</span>
							<span class="mt-1 text-xs text-muted-foreground/70">.nzb or .nzb.gz files</span>
						{/if}
						<input
							type="file"
							accept=".nzb,.nzb.gz"
							class="hidden"
							onchange={(e) => { files = (e.target as HTMLInputElement).files; }}
						/>
					</label>
					<Button
						class="mt-4 w-full bg-primary text-primary-foreground hover:bg-primary/90"
						onclick={submitFile}
						disabled={submitting || !files || files.length === 0}
					>
						{submitting ? 'Uploading...' : 'Upload'}
					</Button>
				</div>
			{:else}
				<div class="mt-4">
					<input
						type="url"
						bind:value={url}
						placeholder="https://example.com/file.nzb"
						class="w-full rounded-lg border border-input px-3 py-2 text-sm bg-transparent focus:border-primary focus:outline-none focus:ring-1 focus:ring-primary text-foreground placeholder-muted-foreground/50"
						onkeydown={(e) => e.key === 'Enter' && submitUrl()}
					/>
					<Button
						class="mt-4 w-full bg-primary text-primary-foreground hover:bg-primary/90"
						onclick={submitUrl}
						disabled={submitting || !url.trim()}
					>
						{submitting ? 'Fetching...' : 'Fetch'}
					</Button>
				</div>
			{/if}
		</div>

		{#if result}
			<p class="mt-3 text-sm {result.ok ? 'text-green-600 dark:text-green-400' : 'text-destructive'} font-semibold">
				{result.message}
			</p>
		{/if}
	</div>
</Modal>
