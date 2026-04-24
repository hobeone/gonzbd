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
		onTestServer
	}: {
		configData: Record<string, any>;
		onAddServer: () => void;
		onEditServer: (server: ServerConfig) => void;
		onDeleteServer: (name: string) => void;
		onTestServer: (server: ServerConfig) => void;
	} = $props();
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

	<div class="space-y-4">
		{#if configData.servers.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
				No servers configured.
			</div>
		{:else}
			<div class="overflow-hidden rounded-md border">
				<table class="w-full text-left text-sm">
					<thead class="bg-gray-50 text-xs uppercase text-gray-500">
						<tr>
							<th class="px-4 py-2">Server / Connection</th>
							<th class="px-4 py-2">Details</th>
							<th class="px-4 py-2 text-right">Actions</th>
						</tr>
					</thead>
					<tbody class="divide-y">
						{#each configData.servers as server}
							<tr class={server.enable ? 'hover:bg-gray-50' : 'bg-gray-50/50 grayscale-[0.5]'}>
								<td class="px-4 py-3">
									<div class="flex items-center gap-2 font-medium">
										{server.name}
										{#if !server.enable}
											<Badge variant="destructive" class="py-0 h-3.5 text-[9px] uppercase tracking-tighter opacity-70">Disabled</Badge>
										{/if}
									</div>
									<div class="mt-0.5 font-mono text-[11px] text-gray-500">
										{server.host}:{server.port}
										{#if server.ssl}
											<span class="ml-1.5 inline-flex items-center rounded bg-blue-50 px-1 py-0 text-[9px] font-bold text-blue-600 ring-1 ring-inset ring-blue-500/20">TLS</span>
										{/if}
									</div>
								</td>
								<td class="px-4 py-3">
									<div class="flex flex-col gap-0.5 text-[11px] text-gray-600">
										<div class="flex items-center gap-1">
											<span class="text-gray-400 w-12 shrink-0">User:</span>
											<span class="truncate max-w-[120px] font-medium">{server.username || 'anonymous'}</span>
										</div>
										<div class="flex items-center gap-1">
											<span class="text-gray-400 w-12 shrink-0">Priority:</span>
											<span class="font-bold">{server.priority}</span>
										</div>
										<div class="flex items-center gap-1">
											<span class="text-gray-400 w-12 shrink-0">Pool:</span>
											<span>{server.connections} conns</span>
										</div>
									</div>
								</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-0.5">
										<Button variant="ghost" size="xs" onclick={() => onTestServer(server)} title="Test connection">Test</Button>
										<Button variant="ghost" size="xs" onclick={() => onEditServer(server)} title="Edit server">Edit</Button>
										<Button variant="ghost" size="xs" class="text-red-600" onclick={() => onDeleteServer(server.name)} title="Delete server">Delete</Button>
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
