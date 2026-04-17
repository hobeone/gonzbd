<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Tabs } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { uploadNzb, postAction } from '$lib/api';
	import { getApiKey } from '$lib/stores/apikey.svelte';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let activeTab = $state('file');
	let url = $state('');
	let files = $state<FileList | null>(null);
	let dragging = $state(false);
	let submitting = $state(false);
	let result = $state<{ ok: boolean; message: string } | null>(null);

	function reset() {
		url = '';
		files = null;
		dragging = false;
		submitting = false;
		result = null;
	}

	async function submitFile() {
		if (!files || files.length === 0) return;
		submitting = true;
		result = null;
		try {
			await uploadNzb(getApiKey(), files[0]);
			result = { ok: true, message: `Added ${files[0].name}` };
			files = null;
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
			await postAction(getApiKey(), 'addurl', { name: trimmed });
			result = { ok: true, message: 'URL submitted' };
			url = '';
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
		<Dialog.Overlay class="fixed inset-0 z-50 bg-black/50" />
		<Dialog.Content
			class="fixed left-1/2 top-1/2 z-50 w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-lg"
			ondrop={handleDrop}
			ondragover={handleDragOver}
			ondragleave={() => (dragging = false)}
		>
			<Dialog.Title class="text-lg font-semibold">Add NZB</Dialog.Title>
			<Dialog.Description class="mt-1 text-sm text-gray-500">
				Upload an NZB file or paste a URL.
			</Dialog.Description>

			<Tabs.Root bind:value={activeTab} class="mt-4">
				<Tabs.List class="flex gap-1 border-b">
					<Tabs.Trigger
						value="file"
						class="border-b-2 px-3 py-1.5 text-sm font-medium data-[state=active]:border-blue-600 data-[state=active]:text-blue-600 data-[state=inactive]:border-transparent data-[state=inactive]:text-gray-500"
					>
						File
					</Tabs.Trigger>
					<Tabs.Trigger
						value="url"
						class="border-b-2 px-3 py-1.5 text-sm font-medium data-[state=active]:border-blue-600 data-[state=active]:text-blue-600 data-[state=inactive]:border-transparent data-[state=inactive]:text-gray-500"
					>
						URL
					</Tabs.Trigger>
				</Tabs.List>

				<Tabs.Content value="file" class="mt-4">
					<label
						class="flex cursor-pointer flex-col items-center justify-center rounded-lg border-2 border-dashed p-8 transition-colors
						{dragging ? 'border-blue-500 bg-blue-50' : 'border-gray-300 hover:border-gray-400'}"
					>
						<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 20 20" fill="currentColor" class="mb-2 size-8 text-gray-400">
							<path d="M9.25 13.25a.75.75 0 0 0 1.5 0V4.636l2.955 3.129a.75.75 0 0 0 1.09-1.03l-4.25-4.5a.75.75 0 0 0-1.09 0l-4.25 4.5a.75.75 0 1 0 1.09 1.03L9.25 4.636v8.614Z" />
							<path d="M3.5 12.75a.75.75 0 0 0-1.5 0v2.5A2.75 2.75 0 0 0 4.75 18h10.5A2.75 2.75 0 0 0 18 15.25v-2.5a.75.75 0 0 0-1.5 0v2.5c0 .69-.56 1.25-1.25 1.25H4.75c-.69 0-1.25-.56-1.25-1.25v-2.5Z" />
						</svg>
						{#if files && files.length > 0}
							<span class="text-sm font-medium text-gray-900">{files[0].name}</span>
							<span class="mt-1 text-xs text-gray-500">{(files[0].size / 1024).toFixed(1)} KB</span>
						{:else}
							<span class="text-sm text-gray-600">Drop NZB file here or click to browse</span>
							<span class="mt-1 text-xs text-gray-400">.nzb files only</span>
						{/if}
						<input
							type="file"
							accept=".nzb,.nzb.gz"
							class="hidden"
							onchange={(e) => { files = (e.target as HTMLInputElement).files; }}
						/>
					</label>
					<Button
						class="mt-4 w-full"
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
						class="w-full rounded-md border px-3 py-2 text-sm focus:border-blue-500 focus:outline-none focus:ring-1 focus:ring-blue-500"
						onkeydown={(e) => e.key === 'Enter' && submitUrl()}
					/>
					<Button
						class="mt-4 w-full"
						onclick={submitUrl}
						disabled={submitting || !url.trim()}
					>
						{submitting ? 'Fetching...' : 'Fetch'}
					</Button>
				</Tabs.Content>
			</Tabs.Root>

			{#if result}
				<p class="mt-3 text-sm {result.ok ? 'text-green-600' : 'text-red-600'}">
					{result.message}
				</p>
			{/if}
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
