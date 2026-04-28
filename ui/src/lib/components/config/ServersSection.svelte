<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import { Badge } from '$lib/components/ui/badge';
	import type { ServerConfig } from '$lib/types';

	let {
		configData,
		onAddServer,
		onEditServer,
		onDeleteServer,
		onTestServer,
		onToggleServer
	}: {
		configData: Record<string, any>;
		onAddServer: () => void;
		onEditServer: (server: ServerConfig) => void;
		onDeleteServer: (name: string) => void;
		onTestServer: (server: ServerConfig) => void;
		onToggleServer: (server: ServerConfig, enabled: boolean) => void;
	} = $props();

	function allDisabled(): boolean {
		const servers: ServerConfig[] = configData.servers ?? [];
		return servers.length > 0 && servers.every((s) => !s.enable);
	}
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">Usenet Servers</h3>
			<p class="text-sm text-muted-foreground">Manage your NNTP server connections.</p>
		</div>
		<Button size="sm" onclick={onAddServer}>+ Add Server</Button>
	</div>
	<Separator />

	{#if allDisabled()}
		<div class="flex items-center gap-2.5 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-700 dark:bg-amber-950 dark:text-amber-200">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16" fill="currentColor" class="size-4 flex-shrink-0 text-amber-500 dark:text-amber-400">
				<path fill-rule="evenodd" d="M6.701 2.25c.577-1 2.02-1 2.598 0l5.196 9a1.5 1.5 0 0 1-1.299 2.25H2.804a1.5 1.5 0 0 1-1.3-2.25l5.197-9ZM8 5a.75.75 0 0 1 .75.75v2a.75.75 0 0 1-1.5 0v-2A.75.75 0 0 1 8 5Zm0 6.5a.75.75 0 1 0 0-1.5.75.75 0 0 0 0 1.5Z" clip-rule="evenodd" />
			</svg>
			<span>All servers are disabled. Downloads will not start until at least one server is enabled.</span>
		</div>
	{/if}

	<div class="space-y-4">
		{#if configData.servers.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500 dark:text-gray-400">
				No servers configured.
			</div>
		{:else}
			<div class="overflow-hidden rounded-md border">
				<table class="w-full text-left text-sm">
					<thead class="bg-gray-50 dark:bg-gray-800 text-xs uppercase text-gray-500 dark:text-gray-400">
						<tr>
							<th class="w-10 px-4 py-2"></th>
							<th class="px-4 py-2">Server / Connection</th>
							<th class="px-4 py-2">Details</th>
							<th class="px-4 py-2 text-right">Actions</th>
						</tr>
					</thead>
					<tbody class="divide-y">
						{#each configData.servers as server}
							<tr class={server.enable ? 'hover:bg-gray-50 dark:hover:bg-gray-800' : 'bg-gray-50/50 dark:bg-gray-800/50 opacity-60'}>
								<td class="px-4 py-3">
									<label class="flex items-center cursor-pointer" title={server.enable ? 'Disable server' : 'Enable server'}>
										<input
											type="checkbox"
											checked={server.enable}
											onchange={() => onToggleServer(server, !server.enable)}
											class="size-4 rounded border-gray-300 dark:border-gray-600 text-blue-600 focus:ring-blue-500 cursor-pointer"
										/>
									</label>
								</td>
								<td class="px-4 py-3">
									<div class="flex items-center gap-2 font-medium">
										{server.name}
										{#if !server.enable}
											<Badge variant="destructive" class="py-0 h-3.5 text-[9px] uppercase tracking-tighter opacity-70">Disabled</Badge>
										{/if}
									</div>
									<div class="mt-0.5 font-mono text-[11px] text-gray-500 dark:text-gray-400">
										{server.host}:{server.port}
										{#if server.ssl}
											<span class="ml-1.5 inline-flex items-center rounded bg-blue-50 dark:bg-blue-950 px-1 py-0 text-[9px] font-bold text-blue-600 dark:text-blue-400 ring-1 ring-inset ring-blue-500/20">TLS</span>
										{/if}
									</div>
								</td>
								<td class="px-4 py-3">
									<div class="flex flex-col gap-0.5 text-[11px] text-gray-600 dark:text-gray-400">
										<div class="flex items-center gap-1">
											<span class="text-gray-400 dark:text-gray-500 w-12 shrink-0">User:</span>
											<span class="truncate max-w-[120px] font-medium">{server.username || 'anonymous'}</span>
										</div>
										<div class="flex items-center gap-1">
											<span class="text-gray-400 dark:text-gray-500 w-12 shrink-0">Priority:</span>
											<span class="font-bold">{server.priority}</span>
										</div>
										<div class="flex items-center gap-1">
											<span class="text-gray-400 dark:text-gray-500 w-12 shrink-0">Pool:</span>
											<span>{server.connections} conns</span>
										</div>
									</div>
								</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-0.5">
										<Button variant="ghost" size="xs" onclick={() => onTestServer(server)} title="Test connection">Test</Button>
										<Button variant="ghost" size="xs" onclick={() => onEditServer(server)} title="Edit server">Edit</Button>
										<Button variant="ghost" size="xs" class="text-red-600 dark:text-red-400" onclick={() => onDeleteServer(server.name)} title="Delete server">Delete</Button>
									</div>
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</div>
</section>
