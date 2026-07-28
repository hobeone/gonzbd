<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Tabs } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { uploadNzb, postAction, fetchCategories } from '$lib/api';
	import { FileUp } from '@lucide/svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let activeTab = $state('file');
	let url = $state('');
	let category = $state('*');
	let password = $state('');
	let categories = $state.raw<string[]>(['*']);
	let files = $state<FileList | null>(null);
	let dragging = $state(false);
	let submitting = $state(false);
	let result = $state<{ ok: boolean; message: string } | null>(null);

	$effect(() => {
		if (open) {
			fetchCategories().then((cats) => {
				// Don't re-add * if it's already there from backend
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
		dragging = false;
		submitting = false;
		result = null;
	}

	async function submitFile() {
		if (!files || files.length === 0) return;
		submitting = true;
		result = null;
		try {
			const params: Record<string, string> = {};
			if (category !== '*') params.cat = category;
			if (password) params.password = password;

			await uploadNzb(files[0], params);
			open = false;
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

			await postAction('addurl', params);
			open = false;
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

<Dialog.Root bind:open onOpenChange={(o) => { if (o) reset(); }}>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50 backdrop-blur-xs" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-3xl border border-m3-outline/20 bg-m3-surface p-6 shadow-m3-3 text-m3-on-surface outline-none"
			ondrop={handleDrop}
			ondragover={handleDragOver}
			ondragleave={() => (dragging = false)}
		>
			<Dialog.Title class="text-lg font-semibold tracking-tight">Add NZB</Dialog.Title>
			<Dialog.Description class="mt-1 text-sm text-m3-on-surface-variant/80">
				Upload an NZB file or paste a URL.
			</Dialog.Description>

			<Tabs.Root bind:value={activeTab} class="mt-4">
				<Tabs.List class="flex gap-1 border-b border-m3-outline/10">
					<Tabs.Trigger
						value="file"
						class="border-b-2 px-4 py-2 text-sm font-semibold tracking-wide transition-all data-[state=active]:border-m3-primary data-[state=active]:text-m3-primary data-[state=inactive]:border-transparent data-[state=inactive]:text-m3-on-surface-variant/75"
					>
						File
					</Tabs.Trigger>
					<Tabs.Trigger
						value="url"
						class="border-b-2 px-4 py-2 text-sm font-semibold tracking-wide transition-all data-[state=active]:border-m3-primary data-[state=active]:text-m3-primary data-[state=inactive]:border-transparent data-[state=inactive]:text-m3-on-surface-variant/75"
					>
						URL
					</Tabs.Trigger>
				</Tabs.List>

				<div class="mt-4 grid grid-cols-2 gap-4">
					<div class="space-y-1.5">
						<label for="category" class="text-xs font-semibold text-m3-on-surface-variant">Category</label>
						<select
							id="category"
							bind:value={category}
							class="flex h-9 w-full rounded-lg border border-m3-outline bg-transparent px-3 py-1 text-sm shadow-sm transition-colors focus:border-m3-primary focus:outline-none focus:ring-1 focus:ring-m3-primary disabled:cursor-not-allowed disabled:opacity-50"
						>
							{#each categories as cat (cat)}
								<option value={cat}>{cat}</option>
							{/each}
						</select>
					</div>
					<div class="space-y-1.5">
						<label for="password" class="text-xs font-semibold text-m3-on-surface-variant">Password</label>
						<Input
							id="password"
							type="text"
							placeholder="Optional"
							bind:value={password}
							class="h-9"
						/>
					</div>
				</div>

				<Tabs.Content value="file" class="mt-4">
					<label
						class="flex cursor-pointer flex-col items-center justify-center rounded-3xl border-2 border-dashed p-8 transition-colors
						{dragging ? 'border-m3-primary bg-m3-primary/10' : 'border-m3-outline/40 hover:border-m3-primary/70 bg-transparent'}"
					>
						<FileUp class="size-10 text-muted-foreground/80 mb-2" />
						{#if files && files.length > 0}
							<span class="block w-full max-w-[200px] sm:max-w-xs text-sm font-medium text-m3-on-surface truncate text-center" title={files[0].name}>{files[0].name}</span>
							<span class="mt-1 text-xs text-m3-on-surface-variant/85">{(files[0].size / 1024).toFixed(1)} KB</span>
						{:else}
							<span class="text-sm text-m3-on-surface-variant text-center font-medium">Drop NZB file here or click to browse</span>
							<span class="mt-1 text-xs text-m3-on-surface-variant/70">.nzb or .nzb.gz files</span>
						{/if}
						<input
							type="file"
							accept=".nzb,.nzb.gz"
							class="hidden"
							onchange={(e) => { files = (e.target as HTMLInputElement).files; }}
						/>
					</label>
					<Button
						class="mt-4 w-full bg-m3-primary text-m3-on-primary hover:bg-m3-primary/90"
						onclick={submitFile}
						disabled={submitting || !files || files.length === 0}
					>
						{submitting ? 'Uploading...' : 'Upload'}
					</Button>
				</Tabs.Content>

				<Tabs.Content value="url" class="mt-4">
					<input
						type="url"
						bind:value={url}
						placeholder="https://example.com/file.nzb"
						class="w-full rounded-lg border border-m3-outline px-3 py-2 text-sm bg-transparent focus:border-m3-primary focus:outline-none focus:ring-1 focus:ring-m3-primary text-m3-on-surface placeholder-m3-on-surface-variant/50"
						onkeydown={(e) => e.key === 'Enter' && submitUrl()}
					/>
					<Button
						class="mt-4 w-full bg-m3-primary text-m3-on-primary hover:bg-m3-primary/90"
						onclick={submitUrl}
						disabled={submitting || !url.trim()}
					>
						{submitting ? 'Fetching...' : 'Fetch'}
					</Button>
				</Tabs.Content>
			</Tabs.Root>

			{#if result}
				<p class="mt-3 text-sm {result.ok ? 'text-green-600 dark:text-green-400' : 'text-destructive'} font-semibold">
					{result.message}
				</p>
			{/if}
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
