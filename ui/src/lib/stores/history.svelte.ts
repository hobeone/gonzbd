import { fetchHistory, postAction } from '$lib/api';
import type { HistoryDetail, HistorySlot } from '$lib/types';

const POLL_INTERVAL = 5000;

let history = $state<HistoryDetail | null>(null);
let error = $state<string | null>(null);
let timer: ReturnType<typeof setInterval> | null = null;

async function poll() {
	try {
		const res = await fetchHistory(0, 50);
		history = res.history;
		error = null;
	} catch (e) {
		error = e instanceof Error ? e.message : String(e);
	}
}

export function startHistoryPolling() {
	if (timer) return;
	poll();
	timer = setInterval(poll, POLL_INTERVAL);
}

export function stopHistoryPolling() {
	if (timer) {
		clearInterval(timer);
		timer = null;
	}
}

export function getHistory(): HistoryDetail | null {
	return history;
}

export function getHistorySlots(): HistorySlot[] {
	return history?.slots ?? [];
}

export function getHistoryError(): string | null {
	return error;
}

export async function deleteHistoryItem(nzoId: string) {
	await postAction('history', { name: 'delete', value: nzoId });
	await poll();
}

export async function purgeHistory(deleteFiles: boolean) {
	await postAction('history', {
		name: 'delete',
		value: 'all',
		del_files: deleteFiles ? '1' : '0'
	});
	await poll();
}
