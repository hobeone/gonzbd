import { onMount } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import { getCookie } from '$lib/utils';

export interface WSEvent {
	event: string;
	speed?: number;
	remaining?: number;
}

type Handler = (event: WSEvent) => void;

let socket: WebSocket | null = null;
const handlers = new SvelteSet<Handler>();
let isConnected = $state(false);
let reconnectTimeout: ReturnType<typeof setTimeout> | null = null;
let retryDelay = 1000;

function connect() {
	if (socket || reconnectTimeout) return;

	const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
	let url = `${protocol}//${window.location.host}/api/ws`;

	const apikey = getCookie('sab_apikey');
	if (apikey) {
		url += `?apikey=${apikey}`;
	}

	socket = new WebSocket(url);

	socket.onopen = () => {
		console.log('WebSocket connected');
		isConnected = true;
		retryDelay = 1000;
	};

	socket.onmessage = (event) => {
		try {
			const data: WSEvent = JSON.parse(event.data);
			handlers.forEach((h) => h(data));
		} catch (e) {
			console.error('Failed to parse WS event:', e);
		}
	};

	socket.onclose = () => {
		console.log('WebSocket disconnected');
		isConnected = false;
		socket = null;
		scheduleReconnect();
	};

	socket.onerror = (err) => {
		console.error('WebSocket error:', err);
		socket?.close();
	};
}

function scheduleReconnect() {
	if (reconnectTimeout) return;
	reconnectTimeout = setTimeout(() => {
		reconnectTimeout = null;
		retryDelay = Math.min(retryDelay * 2, 30000);
		connect();
	}, retryDelay);
}

export function subscribeWS(handler: Handler) {
	handlers.add(handler);
	if (!socket) connect();
	return () => {
		handlers.delete(handler);
		if (handlers.size === 0) {
			if (reconnectTimeout) {
				clearTimeout(reconnectTimeout);
				reconnectTimeout = null;
			}
			socket?.close();
			socket = null;
		}
	};
}

export function getWSStatus() {
	return isConnected;
}
