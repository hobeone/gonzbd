import { fetchQueue, postAction } from '$lib/api';
import { getApiKey, hasApiKey } from '$lib/stores/apikey.svelte';
import type { QueueDetail, QueueSlot } from '$lib/types';

const POLL_INTERVAL = 2000;

let queue = $state<QueueDetail | null>(null);
let polling = $state(false);
let error = $state<string | null>(null);
let timer: ReturnType<typeof setInterval> | null = null;

async function poll() {
	if (!hasApiKey()) return;
	try {
		const res = await fetchQueue(getApiKey(), 0, 50);
		queue = res.queue;
		error = null;
	} catch (e) {
		error = e instanceof Error ? e.message : String(e);
	}
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

export async function pauseJob(nzoId: string) {
	await postAction(getApiKey(), 'queue', { name: 'pause', value: nzoId });
	await poll();
}

export async function resumeJob(nzoId: string) {
	await postAction(getApiKey(), 'queue', { name: 'resume', value: nzoId });
	await poll();
}

export async function deleteJob(nzoId: string) {
	await postAction(getApiKey(), 'queue', { name: 'delete', value: nzoId });
	await poll();
}
