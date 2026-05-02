import { SvelteSet } from 'svelte/reactivity';
import { getCookie } from '$lib/utils';

export interface WSEvent {
	event: string;
	speed?: number;
	remaining?: number;
	speed_limit?: number;
	bandwidth_max?: number;
	bandwidth_perc?: number;
}

type Handler = (event: WSEvent) => void;

class WebSocketStore {
	#socket: WebSocket | null = null;
	#handlers = new SvelteSet<Handler>();
	#isConnected = $state(false);
	#reconnectTimeout: ReturnType<typeof setTimeout> | null = null;
	#retryDelay = 1000;

	get isConnected() { return this.#isConnected; }

	#connect() {
		if (this.#socket || this.#reconnectTimeout) return;

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
			this.#retryDelay = 1000;
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
			this.#scheduleReconnect();
		};

		this.#socket.onerror = (err) => {
			console.error('WebSocket error:', err);
			this.#socket?.close();
		};
	}

	#scheduleReconnect() {
		if (this.#reconnectTimeout) return;
		this.#reconnectTimeout = setTimeout(() => {
			this.#reconnectTimeout = null;
			this.#retryDelay = Math.min(this.#retryDelay * 2, 30000);
			this.#connect();
		}, this.#retryDelay);
	}

	subscribe(handler: Handler) {
		this.#handlers.add(handler);
		if (!this.#socket) this.#connect();
		return () => {
			this.#handlers.delete(handler);
			if (this.#handlers.size === 0) {
				if (this.#reconnectTimeout) {
					clearTimeout(this.#reconnectTimeout);
					this.#reconnectTimeout = null;
				}
				this.#socket?.close();
				this.#socket = null;
			}
		};
	}
}

const store = new WebSocketStore();

// Exported wrapper functions to maintain API compatibility with components
export const subscribeWS = (handler: Handler) => store.subscribe(handler);
export const getWSStatus = () => store.isConnected;
