<script lang="ts">
	import { isConnected, isAuthExpired, getLastError, getNextRetryAt, isProbing, retryNow } from '$lib/stores/connection.svelte';

	// Live countdown: updates every 100ms for smooth countdown display.
	let now = $state(Date.now());
	let countdownInterval: ReturnType<typeof setInterval> | null = null;

	$effect(() => {
		if (!isConnected()) {
			countdownInterval = setInterval(() => {
				now = Date.now();
			}, 100);
			return () => {
				if (countdownInterval) {
					clearInterval(countdownInterval);
					countdownInterval = null;
				}
			};
		} else {
			if (countdownInterval) {
				clearInterval(countdownInterval);
				countdownInterval = null;
			}
		}
	});

	const secondsUntilRetry = $derived.by(() => {
		const target = getNextRetryAt();
		if (!target) return 0;
		return Math.max(0, Math.ceil((target - now) / 1000));
	});

	// Brief "Reconnected" flash.
	let showReconnected = $state(false);
	let wasDisconnected = $state(false);

	$effect(() => {
		if (!isConnected()) {
			wasDisconnected = true;
		} else if (wasDisconnected) {
			wasDisconnected = false;
			showReconnected = true;
			setTimeout(() => {
				showReconnected = false;
			}, 1500);
		}
	});
</script>

{#if !isConnected()}
	{#if isAuthExpired()}
		<!-- Session Expired banner -->
		<div
			class="mx-auto w-full max-w-7xl px-4 pt-2 animate-in fade-in slide-in-from-top-2"
			role="alert"
		>
			<div
				class="flex items-center gap-3 rounded-lg border border-purple-500 bg-purple-50 px-4 py-3 shadow-sm dark:border-purple-800 dark:bg-purple-950/40"
			>
				<!-- Pulsing lock icon -->
				<svg
					xmlns="http://www.w3.org/2000/svg"
					viewBox="0 0 24 24"
					fill="none"
					stroke="currentColor"
					stroke-width="2"
					stroke-linecap="round"
					stroke-linejoin="round"
					class="size-5 shrink-0 animate-pulse text-purple-600 dark:text-purple-400"
				>
					<rect x="3" y="11" width="18" height="11" rx="2" ry="2" />
					<path d="M7 11V7a5 5 0 0 1 10 0v4" />
				</svg>

				<div class="flex-1 min-w-0">
					<p class="text-sm font-semibold text-purple-900 dark:text-purple-200">
						Session Expired
					</p>
					<p class="text-xs text-purple-700 dark:text-purple-300 flex items-center gap-1.5">
						<svg class="animate-spin h-3 w-3 text-purple-600 dark:text-purple-400" fill="none" viewBox="0 0 24 24">
							<circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
							<path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
						</svg>
						Redirecting to login page…
					</p>
				</div>

				<button
					onclick={() => window.location.reload()}
					class="shrink-0 rounded-md border border-purple-300 bg-purple-100 px-3 py-1.5 text-xs font-medium text-purple-800 transition hover:bg-purple-200 dark:border-purple-700 dark:bg-purple-900/60 dark:text-purple-200 dark:hover:bg-purple-800"
				>
					Log In Now
				</button>
			</div>
		</div>
	{:else}
		<!-- Disconnected banner -->
		<div
			class="mx-auto w-full max-w-7xl px-4 pt-2 animate-in fade-in slide-in-from-top-2"
			role="alert"
		>
			<div
				class="flex items-center gap-3 rounded-lg border border-amber-300 bg-amber-50 px-4 py-3 shadow-sm dark:border-amber-700 dark:bg-amber-950"
			>
			<!-- Pulsing wifi-off icon -->
			<svg
				xmlns="http://www.w3.org/2000/svg"
				viewBox="0 0 24 24"
				fill="none"
				stroke="currentColor"
				stroke-width="2"
				stroke-linecap="round"
				stroke-linejoin="round"
				class="size-5 shrink-0 animate-pulse text-amber-600 dark:text-amber-400"
			>
				<line x1="2" y1="2" x2="22" y2="22" />
				<path d="M8.5 16.5a5 5 0 0 1 7 0" />
				<path d="M2 8.82a15 15 0 0 1 4.17-2.65" />
				<path d="M10.66 5c4.01-.36 8.14.9 11.34 3.76" />
				<path d="M16.85 11.25a10 10 0 0 1 2.22 1.68" />
				<path d="M5 12.86a10 10 0 0 1 5.17-2.89" />
				<line x1="12" y1="20" x2="12.01" y2="20" />
			</svg>

			<div class="flex-1 min-w-0">
				<p class="text-sm font-medium text-amber-800 dark:text-amber-200">
					Unable to reach GoNZBD server
				</p>
				<p class="text-xs text-amber-600 dark:text-amber-400">
					{#if isProbing()}
						Retrying…
					{:else if secondsUntilRetry > 0}
						Retrying in {secondsUntilRetry}s…
					{:else}
						{getLastError() ?? 'Connection lost'}
					{/if}
				</p>
			</div>

			<button
				onclick={retryNow}
				disabled={isProbing()}
				class="shrink-0 rounded-md border border-amber-300 bg-amber-100 px-3 py-1.5 text-xs font-medium text-amber-800 transition hover:bg-amber-200 disabled:opacity-50 dark:border-amber-600 dark:bg-amber-900 dark:text-amber-200 dark:hover:bg-amber-800"
			>
				Retry Now
			</button>
		</div>
	</div>
	{/if}
{:else if showReconnected}
	<!-- Brief reconnected flash -->
	<div class="mx-auto w-full max-w-7xl px-4 pt-2 animate-in fade-in">
		<div
			class="flex items-center gap-2 rounded-lg border border-green-300 bg-green-50 px-4 py-2 dark:border-green-700 dark:bg-green-950"
		>
			<svg
				xmlns="http://www.w3.org/2000/svg"
				viewBox="0 0 20 20"
				fill="currentColor"
				class="size-4 text-green-600 dark:text-green-400"
			>
				<path
					fill-rule="evenodd"
					d="M10 18a8 8 0 1 0 0-16 8 8 0 0 0 0 16Zm3.857-9.809a.75.75 0 0 0-1.214-.882l-3.483 4.79-1.88-1.88a.75.75 0 1 0-1.06 1.061l2.5 2.5a.75.75 0 0 0 1.137-.089l4-5.5Z"
					clip-rule="evenodd"
				/>
			</svg>
			<p class="text-sm text-green-700 dark:text-green-300">Reconnected</p>
		</div>
	</div>
{/if}
