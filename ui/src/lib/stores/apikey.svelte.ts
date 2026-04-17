const STORAGE_KEY = 'sabnzbd-apikey';

let apiKey = $state(localStorage.getItem(STORAGE_KEY) ?? '');

export function getApiKey(): string {
	return apiKey;
}

export function setApiKey(key: string): void {
	apiKey = key;
	localStorage.setItem(STORAGE_KEY, key);
}

export function hasApiKey(): boolean {
	return apiKey.length > 0;
}
