import { SvelteSet } from 'svelte/reactivity';
import { getCookie } from '$lib/utils';
import { reportDisconnect, reportSuccess, onReconnected } from '$lib/stores/connection.svelte';

export interface WSEvent {
	event: string;
	speed?: number;
	remaining?: number;
	speed_limit?: number;
	bandwidth_max?: number;
	bandwidth_perc?: number;
	nzo_id?: string;
	tool?: string;
	line?: string;
	stage?: string;
	servers?: import('$lib/types').ServerSnapshot[];
}

type Handler = (event: WSEvent) => void;

class WebSocketStore {
	#socket: WebSocket | null = null;
	#handlers = new SvelteSet<Handler>();
	#isConnected = $state(false);

	get isConnected() { return this.#isConnected; }

	#connect() {
		if (this.#socket) return;

		const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
		let url = `${protocol}//${window.location.host}/api/ws`;

		const apikey = getCookie('gonzbd_apikey');
		if (apikey) {
			url += `?apikey=${apikey}`;
		}

		this.#socket = new WebSocket(url);

		this.#socket.onopen = () => {
			console.log('WebSocket connected');
			this.#isConnected = true;
			reportSuccess();
		};

		this.#socket.onmessage = (event) => {
			try {
				const data: WSEvent = JSON.parse(event.data);
				this.#handlers.forEach((h) => h(data));
			} catch (e) {
				console.error('Failed to parse WS event:', e);
			}
		};

		this.#socket.onclose = () => {
			console.log('WebSocket disconnected');
			this.#isConnected = false;
			this.#socket = null;
			// ConnectionStore owns reconnection timing — we just report
			// the disconnect and wait for reconnect() to be called.
			reportDisconnect('WebSocket disconnected');
		};

		this.#socket.onerror = (err) => {
			console.error('WebSocket error:', err);
			this.#socket?.close();
		};
	}

	/**
	 * Re-establish the WebSocket connection. Called by ConnectionStore
	 * when the health probe succeeds, or on initial subscribe.
	 */
	reconnect() {
		if (this.#socket) {
			this.#socket.close();
			this.#socket = null;
		}
		if (this.#handlers.size > 0) {
			this.#connect();
		}
	}

	subscribe(handler: Handler) {
		this.#handlers.add(handler);
		if (!this.#socket) this.#connect();
		return () => {
			this.#handlers.delete(handler);
			if (this.#handlers.size === 0) {
				this.#socket?.close();
				this.#socket = null;
			}
		};
	}
}

const store = new WebSocketStore();

// Register with ConnectionStore: when connectivity is restored,
// re-establish the WebSocket.
onReconnected(() => store.reconnect());

// Exported wrapper functions to maintain API compatibility with components
export const subscribeWS = (handler: Handler) => store.subscribe(handler);
export const getWSStatus = () => store.isConnected;
