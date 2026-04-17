<script lang="ts">
	import {
		getSpeedBytesPerSec,
		getSpeedHistory,
		getTotalRemainingBytes,
		formatSpeed,
		formatSize,
		getQueueSlots,
		isPaused
	} from '$lib/stores/queue.svelte';
	import SpeedGraph from './SpeedGraph.svelte';

	function eta(): string {
		const speed = getSpeedBytesPerSec();
		const remaining = getTotalRemainingBytes();
		if (speed <= 0 || remaining <= 0) return '--';
		const seconds = remaining / speed;
		if (seconds < 60) return `${Math.round(seconds)}s`;
		if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
		const h = Math.floor(seconds / 3600);
		const m = Math.round((seconds % 3600) / 60);
		return `${h}h ${m}m`;
	}
</script>

<div class="flex items-center gap-4 border-t bg-white px-4 py-2 text-sm text-gray-600">
	<div class="flex items-center gap-2">
		<SpeedGraph data={getSpeedHistory()} />
		<span class="font-mono font-medium text-gray-900">{formatSpeed(getSpeedBytesPerSec())}</span>
	</div>
	<div class="h-4 w-px bg-gray-200"></div>
	<span>{getQueueSlots().length} item{getQueueSlots().length !== 1 ? 's' : ''}</span>
	<div class="h-4 w-px bg-gray-200"></div>
	<span>{formatSize(getTotalRemainingBytes())} left</span>
	<div class="h-4 w-px bg-gray-200"></div>
	<span>ETA: {eta()}</span>
	{#if isPaused()}
		<span class="ml-auto font-medium text-yellow-600">PAUSED</span>
	{/if}
</div>
