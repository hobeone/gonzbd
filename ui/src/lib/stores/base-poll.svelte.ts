import { subscribeWS, type WSEvent } from './websocket.svelte';
import { onReconnected } from './connection.svelte';

export abstract class BasePollStore {
	protected errorState = $state<string | null>(null);
	protected pollingState = $state(false);
	protected currentPageState = $state(0);
	protected pageLimitState = $state(10);
	protected searchTextState = $state('');

	protected wsCleanup: (() => void) | null = null;
	protected reconnectCleanup: (() => void) | null = null;

	abstract poll(): Promise<void>;
	abstract handleWSEvent(event: WSEvent): void;

	start() {
		if (this.pollingState) return;
		this.pollingState = true;
		this.poll();

		this.wsCleanup = subscribeWS((event) => this.handleWSEvent(event));
		this.reconnectCleanup = onReconnected(() => this.poll());
	}

	stop() {
		if (this.wsCleanup) {
			this.wsCleanup();
			this.wsCleanup = null;
		}
		if (this.reconnectCleanup) {
			this.reconnectCleanup();
			this.reconnectCleanup = null;
		}
		this.pollingState = false;
	}

	setPage(page: number) {
		this.currentPageState = page;
		this.poll();
	}

	setLimit(limit: number) {
		this.pageLimitState = limit;
		this.currentPageState = 0;
		this.poll();
	}

	setSearch(search: string) {
		this.searchTextState = search;
		this.currentPageState = 0;
		this.poll();
	}
}
