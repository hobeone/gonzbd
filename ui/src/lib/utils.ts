import { type ClassValue, clsx } from 'clsx';
import { twMerge } from 'tailwind-merge';

export function getCookie(name: string): string | null {
	const value = `; ${document.cookie}`;
	const parts = value.split(`; ${name}=`);
	if (parts.length === 2) return parts.pop()?.split(';').shift() ?? null;
	return null;
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

/**
 * Format a non-negative whole-second duration as a compact ETA string.
 * Examples: 45 → "45s", 130 → "2m 10s", 7320 → "2h 2m".
 * Returns "" for zero or negative input so callers render nothing when
 * ETA is unknown.
 */
export function formatETA(seconds: number): string {
	if (!seconds || seconds < 0) return '';
	if (seconds < 60) return `${Math.floor(seconds)}s`;
	if (seconds < 3600) {
		const m = Math.floor(seconds / 60);
		const s = Math.floor(seconds % 60);
		return s > 0 ? `${m}m ${s}s` : `${m}m`;
	}
	const h = Math.floor(seconds / 3600);
	const m = Math.floor((seconds % 3600) / 60);
	return m > 0 ? `${h}h ${m}m` : `${h}h`;
}

export function cn(...inputs: ClassValue[]) {
	return twMerge(clsx(inputs));
}

export type WithElementRef<T> = T & { ref?: HTMLElement | null };

export type WithoutChildrenOrChild<T> = Omit<T, 'children' | 'child'>;

export type WithoutChild<T> = Omit<T, 'child'>;

export function getRedirectUrl(res: Response, requestedUrl: string): string | null {
	if (!res || !res.url) return null;

	const reqURL = new URL(requestedUrl, window.location.origin);
	const resURL = new URL(res.url);

	const crossOriginRedirect = resURL.origin !== reqURL.origin;
	const sameOriginAuthRedirect = res.redirected && !resURL.pathname.startsWith('/api');

	if (crossOriginRedirect || sameOriginAuthRedirect) {
		const targetURL = new URL(res.url);
		for (const [key, value] of targetURL.searchParams.entries()) {
			if (value.includes('/api')) {
				targetURL.searchParams.set(key, window.location.href);
			}
		}
		return targetURL.href;
	}
	return null;
}

