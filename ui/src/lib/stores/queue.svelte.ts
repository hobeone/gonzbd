import { fetchQueue, postAction } from '$lib/api';
import type { QueueDetail, QueueSlot } from '$lib/types';

const POLL_INTERVAL = 2000;
const SPEED_HISTORY_SIZE = 60;

let queue = $state<QueueDetail | null>(null);
let polling = $state(false);
let error = $state<string | null>(null);
let timer: ReturnType<typeof setInterval> | null = null;

let prevRemainingBytes = $state<number | null>(null);
let prevPollTime = $state<number | null>(null);
let speedBytesPerSec = $state(0);
let speedHistory = $state<number[]>([]);

async function poll() {
	try {
		const res = await fetchQueue(0, 50);
		const now = Date.now();
		const newRemaining = res.queue.slots.reduce((sum, s) => sum + s.remaining_bytes, 0);

		if (prevRemainingBytes !== null && prevPollTime !== null) {
			const dt = (now - prevPollTime) / 1000;
			if (dt > 0) {
				const bytesDownloaded = prevRemainingBytes - newRemaining;
				speedBytesPerSec = Math.max(0, bytesDownloaded / dt);
			}
		}

		prevRemainingBytes = newRemaining;
		prevPollTime = now;
		queue = res.queue;
		error = null;

		speedHistory = [...speedHistory.slice(-(SPEED_HISTORY_SIZE - 1)), speedBytesPerSec];
	} catch (e) {
		error = e instanceof Error ? e.message : String(e);
	}
}

export async function refreshQueue() {
	await poll();
}

export function startPolling() {
	if (timer) return;
	polling = true;
	poll();
	timer = setInterval(poll, POLL_INTERVAL);
}

export function stopPolling() {
	if (timer) {
		clearInterval(timer);
		timer = null;
	}
	polling = false;
}

export function getQueue(): QueueDetail | null {
	return queue;
}

export function getQueueSlots(): QueueSlot[] {
	return queue?.slots ?? [];
}

export function isPaused(): boolean {
	return queue?.paused ?? false;
}

export function getError(): string | null {
	return error;
}

export function isPolling(): boolean {
	return polling;
}

export function getSpeedBytesPerSec(): number {
	return speedBytesPerSec;
}

export function getSpeedHistory(): number[] {
	return speedHistory;
}

export function getTotalRemainingBytes(): number {
	if (!queue) return 0;
	return queue.slots.reduce((sum, s) => sum + s.remaining_bytes, 0);
}

export function formatSpeed(bps: number): string {
	if (bps < 1024) return `${Math.round(bps)} B/s`;
	if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`;
	return `${(bps / (1024 * 1024)).toFixed(1)} MB/s`;
}

export function formatSize(bytes: number): string {
	if (bytes < 1024) return `${bytes} B`;
	if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
	if (bytes < 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
	return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
}

export async function pauseJob(nzoId: string) {
	await postAction('queue', { name: 'pause', value: nzoId });
	await poll();
}

export async function resumeJob(nzoId: string) {
	await postAction('queue', { name: 'resume', value: nzoId });
	await poll();
}

export async function deleteJob(nzoId: string, deleteFiles = false) {
	const params: Record<string, string> = { name: 'delete', value: nzoId };
	if (deleteFiles) {
		params.delete_files = '1';
	}
	await postAction('queue', params);
	await poll();
}
