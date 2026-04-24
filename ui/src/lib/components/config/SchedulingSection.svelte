<script lang="ts">
	import { Separator } from '$lib/components/ui/separator';
	import { Button } from '$lib/components/ui/button';
	import type { ScheduleConfig } from '$lib/types';

	let {
		configData,
		onAddSchedule,
		onEditSchedule,
		onDeleteSchedule
	}: {
		configData: Record<string, any>;
		onAddSchedule: () => void;
		onEditSchedule: (sched: ScheduleConfig) => void;
		onDeleteSchedule: (name: string) => void;
	} = $props();
</script>

<section class="space-y-6">
	<div class="flex items-center justify-between">
		<div>
			<h3 class="text-lg font-medium">Schedules</h3>
			<p class="text-sm text-muted-foreground">Automated actions based on time.</p>
		</div>
		<Button size="sm" onclick={onAddSchedule}>+ Add Schedule</Button>
	</div>
	<Separator />

	<div class="space-y-4">
		{#if configData.schedules.length === 0}
			<div class="rounded-lg border border-dashed p-8 text-center text-sm text-gray-500">
				No schedules configured.
			</div>
		{:else}
			<div class="overflow-hidden rounded-md border">
				<table class="w-full text-left text-sm">
					<thead class="bg-gray-50 text-xs uppercase text-gray-500">
						<tr>
							<th class="px-4 py-2">Name</th>
							<th class="px-4 py-2">Time</th>
							<th class="px-4 py-2">Action</th>
							<th class="px-4 py-2 text-right">Actions</th>
						</tr>
					</thead>
					<tbody class="divide-y">
						{#each configData.schedules as sched}
							<tr class={sched.enabled ? '' : 'opacity-50'}>
								<td class="px-4 py-3 font-medium">{sched.name}</td>
								<td class="px-4 py-3 text-gray-600 font-mono text-xs">{sched.hour}:{sched.minute} ({sched.dayofweek})</td>
								<td class="px-4 py-3 uppercase text-xs font-bold">{sched.action}</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-1">
										<Button variant="ghost" size="xs" onclick={() => onEditSchedule(sched)}>Edit</Button>
										<Button variant="ghost" size="xs" class="text-red-600" onclick={() => onDeleteSchedule(sched.name)}>Delete</Button>
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
