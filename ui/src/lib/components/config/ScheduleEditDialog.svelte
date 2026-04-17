<script lang="ts">
	import { Dialog } from 'bits-ui';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import type { ScheduleConfig } from '$lib/types';

	let {
		open = $bindable(false),
		schedule = null,
		onsave
	}: {
		open?: boolean;
		schedule?: ScheduleConfig | null;
		onsave: (s: ScheduleConfig) => void;
	} = $props();

	let draft = $state<ScheduleConfig>({
		name: '',
		enabled: true,
		action: 'pause',
		arguments: '',
		minute: '*',
		hour: '*',
		dayofweek: '*'
	});

	const actions = [
		{ value: 'pause', label: 'Pause Downloader' },
		{ value: 'resume', label: 'Resume Downloader' },
		{ value: 'pause_all', label: 'Pause All' },
		{ value: 'speedlimit', label: 'Set Speed Limit' },
		{ value: 'shutdown', label: 'Shutdown' },
		{ value: 'restart', label: 'Restart' },
		{ value: 'rss_scan', label: 'Trigger RSS Scan' },
		{ value: 'scan_folder', label: 'Scan Watched Folder' }
	];

	$effect(() => {
		if (open) {
			if (schedule) {
				draft = { ...schedule };
			} else {
				draft = {
					name: '',
					enabled: true,
					action: 'pause',
					arguments: '',
					minute: '*',
					hour: '*',
					dayofweek: '*'
				};
			}
		}
	});

	function handleSave() {
		if (!draft.name || !draft.action) return;
		onsave(draft);
		open = false;
	}
</script>

<Dialog.Root bind:open>
	<Dialog.Portal>
		<Dialog.Overlay class="fixed inset-0 z-[60] bg-black/50" />
		<Dialog.Content class="fixed left-1/2 top-1/2 z-[70] w-full max-w-md -translate-x-1/2 -translate-y-1/2 rounded-lg border bg-white p-6 shadow-xl">
			<Dialog.Title class="text-lg font-semibold">
				{schedule ? 'Edit Schedule' : 'Add Schedule'}
			</Dialog.Title>

			<div class="mt-4 space-y-4">
				<div class="space-y-1.5">
					<label for="sched-name" class="text-sm font-medium">Schedule Name</label>
					<Input id="sched-name" bind:value={draft.name} placeholder="e.g. Nightly Pause" disabled={!!schedule} />
				</div>

				<div class="space-y-1.5">
					<label for="sched-action" class="text-sm font-medium">Action</label>
					<select id="sched-action" bind:value={draft.action} class="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring">
						{#each actions as a}
							<option value={a.value}>{a.label}</option>
						{/each}
					</select>
				</div>

				{#if draft.action === 'speedlimit'}
					<div class="space-y-1.5">
						<label for="sched-args" class="text-sm font-medium">Arguments (e.g. 500K or 50%)</label>
						<Input id="sched-args" bind:value={draft.arguments} />
					</div>
				{/if}

				<div class="grid grid-cols-3 gap-3">
					<div class="space-y-1.5">
						<label for="sched-hour" class="text-sm font-medium">Hour (0-23)</label>
						<Input id="sched-hour" bind:value={draft.hour} placeholder="*" />
					</div>
					<div class="space-y-1.5">
						<label for="sched-min" class="text-sm font-medium">Minute (0-59)</label>
						<Input id="sched-min" bind:value={draft.minute} placeholder="*" />
					</div>
					<div class="space-y-1.5">
						<label for="sched-dow" class="text-sm font-medium">Day (1-7, 1=Mon)</label>
						<Input id="sched-dow" bind:value={draft.dayofweek} placeholder="*" />
					</div>
				</div>

				<div class="flex items-center gap-2 py-2">
					<input id="sched-enabled" type="checkbox" bind:checked={draft.enabled} class="rounded border-gray-300" />
					<label for="sched-enabled" class="text-sm font-medium cursor-pointer">Enabled</label>
				</div>
			</div>

			<div class="mt-6 flex justify-end gap-3">
				<Button variant="ghost" onclick={() => (open = false)}>Cancel</Button>
				<Button onclick={handleSave} disabled={!draft.name}>Save</Button>
			</div>
		</Dialog.Content>
	</Dialog.Portal>
</Dialog.Root>
