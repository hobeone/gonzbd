/**
 * Dynamic favicon generator.
 *
 * Produces inline SVG data URIs for three app states so the browser tab
 * icon gives at-a-glance status:
 *   - downloading → green pulsing arrow
 *   - paused → amber pause bars
 *   - idle → gray circle
 */

export type AppState = 'downloading' | 'paused' | 'idle';

const FAVICONS: Record<AppState, string> = {
  downloading: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
    <defs>
      <linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="#22c55e"/>
        <stop offset="100%" stop-color="#16a34a"/>
      </linearGradient>
    </defs>
    <circle cx="16" cy="16" r="15" fill="url(#g)"/>
    <path d="M16 6v14M10 16l6 6 6-6" stroke="#fff" stroke-width="3" stroke-linecap="round" stroke-linejoin="round" fill="none"/>
  </svg>`,

  paused: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
    <defs>
      <linearGradient id="a" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="#f59e0b"/>
        <stop offset="100%" stop-color="#d97706"/>
      </linearGradient>
    </defs>
    <circle cx="16" cy="16" r="15" fill="url(#a)"/>
    <rect x="10" y="9" width="4" height="14" rx="1" fill="#fff"/>
    <rect x="18" y="9" width="4" height="14" rx="1" fill="#fff"/>
  </svg>`,

  idle: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">
    <defs>
      <linearGradient id="i" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="#6b7280"/>
        <stop offset="100%" stop-color="#4b5563"/>
      </linearGradient>
    </defs>
    <circle cx="16" cy="16" r="15" fill="url(#i)"/>
    <circle cx="16" cy="16" r="5" fill="#fff" opacity="0.8"/>
  </svg>`,
};

/** Returns an SVG data URI suitable for `<link rel="icon" href="...">`. */
export function faviconForState(state: AppState): string {
  const svg = FAVICONS[state];
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}
