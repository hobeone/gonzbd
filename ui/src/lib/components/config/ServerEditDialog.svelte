<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Badge } from '$lib/components/ui/badge';
	import type { ServerConfig } from '$lib/types';
	import { postAction } from '$lib/api';

	let {
		open = $bindable(false),
		server = null,
		onsave
	}: {
		open?: boolean;
		server?: ServerConfig | null;
		onsave: (s: ServerConfig, originalName?: string) => void;
	} = $props();

	let draft = $state<ServerConfig>({
		name: '',
		host: '',
		port: 119,
		username: '',
		password: '',
		connections: 8,
		ssl: false,
		ssl_verify: 2,
		ssl_ciphers: '',
		priority: 0,
		required: false,
		optional: true,
		retention: 0,
		timeout: 60,
		pipelining_requests: 2,
		enable: true
	});

	let originalName = '';
	let testing = $state(false);
	let testResult = $state<{ ok: boolean; message: string } | null>(null);

	$effect(() => {
		if (open) {
			if (server) {
				draft = { ...server };
				originalName = server.name;
			} else {
				originalName = '';
				draft = {
					name: '',
					host: '',
					port: 119,
					username: '',
					password: '',
					connections: 8,
					ssl: false,
					ssl_verify: 2,
					ssl_ciphers: '',
					priority: 0,
					required: false,
					optional: true,
					retention: 0,
					timeout: 60,
					pipelining_requests: 2,
					enable: true
				};
			}
			testResult = null;
		}
	});

	async function testServer() {
		testing = true;
		testResult = null;
		try {
			const res = await postAction('config', {
				name: 'test_server',
				host: draft.host,
				port: String(draft.port),
				username: draft.username,
				password: draft.password,
				ssl: draft.ssl ? '1' : '0',
				ssl_verify: String(draft.ssl_verify)
			});
			const r = (res as any).result;
			if (r && typeof r.passed === 'boolean') {
				testResult = { ok: r.passed, message: r.message };
			} else {
				testResult = { ok: true, message: 'Connection successful!' };
			}
		} catch (e) {
			testResult = { ok: false, message: e instanceof Error ? e.message : String(e) };
		} finally {
			testing = false;
		}
	}

	function handleSave() {
		if (!draft.host || !draft.name) return;
		onsave(draft, originalName);
		open = false;
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-[60] bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-[70] w-full max-w-lg -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-xl">
			<Dialog.Title class="text-lg font-semibold">
				{server ? 'Edit Server' : 'Add Server'}
			</Dialog.Title>
			
			<div class="mt-4 grid grid-cols-2 gap-4">
				<div class="col-span-2 space-y-1.5">
					<label for="server-name" class="text-sm font-medium">Server Name</label>
					<Input id="server-name" bind:value={draft.name} placeholder="e.g. NewsgroupDirect" />
				</div>

				<div class="space-y-1.5">
					<label for="server-host" class="text-sm font-medium">Host</label>
					<Input id="server-host" bind:value={draft.host} placeholder="news.example.com" />
				</div>

				<div class="space-y-1.5">
					<label for="server-port" class="text-sm font-medium">Port</label>
					<Input id="server-port" type="number" bind:value={draft.port} />
				</div>

				<div class="space-y-1.5">
					<label for="server-username" class="text-sm font-medium">Username</label>
					<Input id="server-username" bind:value={draft.username} />
				</div>

				<div class="space-y-1.5">
					<label for="server-password" class="text-sm font-medium">Password</label>
					<Input id="server-password" type="password" bind:value={draft.password} />
				</div>

				<div class="space-y-1.5">
					<label for="server-connections" class="text-sm font-medium">Connections</label>
					<Input id="server-connections" type="number" bind:value={draft.connections} min="1" max="100" />
				</div>

				<div class="space-y-1.5">
					<label for="server-priority" class="text-sm font-medium">Priority</label>
					<Input id="server-priority" type="number" bind:value={draft.priority} min="0" />
				</div>

				<div class="col-span-2 flex items-center gap-6 py-2">
					<label class="flex items-center gap-2 text-sm font-medium cursor-pointer">
						<input type="checkbox" bind:checked={draft.ssl} class="rounded border-gray-300" />
						SSL / TLS
					</label>
					<label class="flex items-center gap-2 text-sm font-medium cursor-pointer">
						<input type="checkbox" bind:checked={draft.enable} class="rounded border-gray-300" />
						Enabled
					</label>
				</div>
			</div>

			{#if testResult}
				<div class="mt-4 rounded-md p-3 text-sm {testResult.ok ? 'bg-green-50 text-green-700' : 'bg-red-50 text-red-700'}">
					{testResult.message}
				</div>
			{/if}

			<div class="mt-6 flex justify-between gap-3">
				<Button variant="outline" onclick={testServer} disabled={testing || !draft.host}>
					{testing ? 'Testing...' : 'Test Server'}
				</Button>
				<div class="flex gap-3">
					<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
					<Button onclick={handleSave} disabled={!draft.host || !draft.name}>Save</Button>
				</div>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
