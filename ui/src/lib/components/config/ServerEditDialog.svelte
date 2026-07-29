<script lang="ts">
	import Modal from '$lib/components/ui/Modal.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Badge } from '$lib/components/ui/badge';
	import type { ServerConfig } from '$lib/types';
	import { postAction } from '$lib/api';

	let {
		open = $bindable(false),
		server = null,
		onsave,
		ondelete
	}: {
		open?: boolean;
		server?: ServerConfig | null;
		onsave: (s: ServerConfig, originalName?: string) => void;
		ondelete?: (name: string) => void;
	} = $props();

	let confirmingDelete = $state(false);

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
		optional: false,
		retention: 0,
		timeout: 60,
		pipelining_requests: 2,
		enable: true
	});

	type ServerRole = 'primary' | 'backup' | 'required';
	function roleOf(d: ServerConfig): ServerRole {
		if (d.required) return 'required';
		if (d.optional) return 'backup';
		return 'primary';
	}
	function setRole(role: ServerRole) {
		draft.required = role === 'required';
		draft.optional = role === 'backup';
	}

	let originalName = $state('');
	let testing = $state(false);
	let testResult = $state<{ ok: boolean; message: string } | null>(null);

	$effect(() => {
		if (open) {
			confirmingDelete = false;
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
					optional: false,
					retention: 0,
					timeout: 60,
					pipelining_requests: 2,
					enable: true
				};
			}
			testResult = null;
		}
	});

	function handleDelete() {
		if (!ondelete || !originalName) return;
		ondelete(originalName);
		open = false;
	}

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

<Modal bind:open class="w-full max-w-lg bg-card text-foreground p-6 border border-border">
	<h2 class="text-lg font-semibold">
		{server ? 'Edit Server' : 'Add Server'}
	</h2>
	
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
			<label for="server-pipelining" class="text-sm font-medium">Pipeline Depth</label>
			<Input id="server-pipelining" type="number" bind:value={draft.pipelining_requests} min="1" max="10" />
			<p class="text-xs text-muted-foreground">Commands in-flight per connection</p>
		</div>

		<div class="space-y-1.5">
			<label for="server-priority" class="text-sm font-medium">Priority</label>
			<Input id="server-priority" type="number" bind:value={draft.priority} min="0" />
		</div>

		<div class="col-span-2 flex items-center gap-6 py-2">
			<label class="flex items-center gap-2 text-sm font-medium cursor-pointer">
				<input type="checkbox" bind:checked={draft.ssl} class="rounded border-border" />
				SSL / TLS
			</label>
			<label class="flex items-center gap-2 text-sm font-medium cursor-pointer">
				<input type="checkbox" bind:checked={draft.enable} class="rounded border-border" />
				Enabled
			</label>
		</div>

		<div class="col-span-2 space-y-1.5">
			<div class="text-sm font-medium">Role</div>
			<div class="flex items-center gap-6 text-sm">
				<label class="flex items-center gap-2 cursor-pointer">
					<input
						type="radio"
						name="server-role"
						value="primary"
						checked={roleOf(draft) === 'primary'}
						onchange={() => setRole('primary')}
					/>
					Primary
				</label>
				<label class="flex items-center gap-2 cursor-pointer">
					<input
						type="radio"
						name="server-role"
						value="backup"
						checked={roleOf(draft) === 'backup'}
						onchange={() => setRole('backup')}
					/>
					Backup
				</label>
				<label class="flex items-center gap-2 cursor-pointer">
					<input
						type="radio"
						name="server-role"
						value="required"
						checked={roleOf(draft) === 'required'}
						onchange={() => setRole('required')}
					/>
					Required
				</label>
			</div>
			<p class="text-xs text-muted-foreground">
				Backup servers can be auto-disabled when failure rate is high. Required servers are never auto-disabled.
			</p>
		</div>
	</div>

	{#if testResult}
		<div class="mt-4 rounded-md p-3 text-sm {testResult.ok ? 'bg-green-50 dark:bg-green-950 text-green-700 dark:text-green-300' : 'bg-red-50 dark:bg-red-950 text-red-700 dark:text-red-300'}">
			{testResult.message}
		</div>
	{/if}

	{#if confirmingDelete}
		<div class="mt-6 flex items-center justify-between rounded-md bg-destructive/10 px-3 py-2">
			<span class="text-sm text-destructive font-medium">Delete <strong>{originalName}</strong>?</span>
			<div class="flex gap-2">
				<Button variant="ghost" size="xs" onclick={() => (confirmingDelete = false)}>No</Button>
				<Button variant="destructive" size="xs" onclick={handleDelete}>Yes, delete</Button>
			</div>
		</div>
	{/if}

	<div class="mt-4 flex items-center">
		<div class="flex-1">
			<Button variant="outline" onclick={testServer} disabled={testing || !draft.host}>
				{testing ? 'Testing...' : 'Test Server'}
			</Button>
		</div>

		{#if server && ondelete && !confirmingDelete}
			<Button variant="ghost" size="sm" class="text-destructive" onclick={() => (confirmingDelete = true)}>Delete</Button>
		{/if}

		<div class="flex flex-1 justify-end gap-3">
			<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
			<Button onclick={handleSave} disabled={!draft.host || !draft.name}>Save</Button>
		</div>
	</div>
</Modal>
