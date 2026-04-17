import { fetchWarnings, postAction } from '$lib/api';

const POLL_INTERVAL = 5000;

let warnings = $state<string[]>([]);
let error = $state<string | null>(null);
let timer: ReturnType<typeof setInterval> | null = null;
let toastMessage = $state<string | null>(null);

async function poll() {
	try {
		const res = await fetchWarnings();
		const prev = warnings.length;
		warnings = res.warnings;
		error = null;
		if (warnings.length > prev && prev > 0) {
			toastMessage = warnings[warnings.length - 1];
			setTimeout(() => (toastMessage = null), 5000);
		}
	} catch (e) {
		error = e instanceof Error ? e.message : String(e);
	}
}

export function startWarningsPolling() {
	if (timer) return;
	poll();
	timer = setInterval(poll, POLL_INTERVAL);
}

export function stopWarningsPolling() {
	if (timer) {
		clearInterval(timer);
		timer = null;
	}
}

export function getWarnings(): string[] {
	return warnings;
}

export function getWarningCount(): number {
	return warnings.length;
}

export function getWarningsError(): string | null {
	return error;
}

export function getToastMessage(): string | null {
	return toastMessage;
}

export function dismissToast() {
	toastMessage = null;
}

export async function clearWarnings() {
	await postAction('warnings', { name: 'clear' });
	await poll();
}
