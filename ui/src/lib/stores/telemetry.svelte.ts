import { postAction } from '$lib/api';
import type { ServerSnapshot } from '$lib/types';
import { subscribeWS, type WSEvent } from './websocket.svelte';

const SPEED_HISTORY_SIZE = 60;

class TelemetryStore {
	#speedBytesPerSec = $state(0);
	#speedHistory = $state<number[]>([]);
	#totalRemainingBytes = $state(0);
	#speedLimitBytesPerSec = $state(0);
	#bandwidthMaxBytesPerSec = $state(0);
	#bandwidthPerc = $state(100);
	#serverStats = $state<ServerSnapshot[]>([]);

	#wsCleanup: (() => void) | null = null;

	get speedBytesPerSec() { return this.#speedBytesPerSec; }
	get speedHistory() { return this.#speedHistory; }
	get totalRemainingBytes() { return this.#totalRemainingBytes; }
	get speedLimitBytesPerSec() { return this.#speedLimitBytesPerSec; }
	get bandwidthMaxBytesPerSec() { return this.#bandwidthMaxBytesPerSec; }
	get bandwidthPerc() { return this.#bandwidthPerc; }
	get serverStats() { return this.#serverStats; }

	start() {
		if (this.#wsCleanup) return;
		this.#wsCleanup = subscribeWS((event) => this.#handleWSEvent(event));
	}

	stop() {
		if (this.#wsCleanup) {
			this.#wsCleanup();
			this.#wsCleanup = null;
		}
	}

	#handleWSEvent(event: WSEvent) {
		if (event.event === 'metrics') {
			this.#speedBytesPerSec = event.speed ?? 0;
			this.#totalRemainingBytes = event.remaining ?? 0;
			this.#speedLimitBytesPerSec = event.speed_limit ?? 0;
			this.#bandwidthMaxBytesPerSec = event.bandwidth_max ?? 0;
			this.#bandwidthPerc = event.bandwidth_perc ?? 100;
			this.#speedHistory = [...this.#speedHistory.slice(-(SPEED_HISTORY_SIZE - 1)), this.#speedBytesPerSec];
			if (event.servers) {
				this.#serverStats = event.servers;
			}
		}
	}

	setTotalRemainingBytes(bytes: number) {
		this.#totalRemainingBytes = bytes;
	}

	async setBandwidthPerc(perc: number) {
		await postAction('set_config', { section: 'downloads', keyword: 'bandwidth_perc', value: String(perc) });
	}
}

const store = new TelemetryStore();

export const getSpeedBytesPerSec = () => store.speedBytesPerSec;
export const getSpeedHistory = () => store.speedHistory;
export const getTotalRemainingBytes = () => store.totalRemainingBytes;
export const getSpeedLimitBytesPerSec = () => store.speedLimitBytesPerSec;
export const getBandwidthMaxBytesPerSec = () => store.bandwidthMaxBytesPerSec;
export const getBandwidthPerc = () => store.bandwidthPerc;
export const getServerStats = () => store.serverStats;
export const setBandwidthPerc = (perc: number) => store.setBandwidthPerc(perc);
export const setTotalRemainingBytes = (bytes: number) => store.setTotalRemainingBytes(bytes);

export const startTelemetry = () => store.start();
export const stopTelemetry = () => store.stop();
