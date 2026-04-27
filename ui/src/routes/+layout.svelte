<script lang="ts">
	import '../app.css';

	let { children } = $props();

	// NOTE: Auto dark-mode detection is intentionally disabled.
	// Many components (SettingsDialog, edit dialogs) use hardcoded
	// light-only colors without dark: variants. Re-enable once full
	// dark mode support is implemented across all components.
</script>

<svelte:head>
	<link rel="icon" href="/favicon.ico" />
	<meta name="application-name" content="SABnzbd-Go" />
</svelte:head>

{@render children()}
