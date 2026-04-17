<script lang="ts">
	import '../app.css';

	let { children } = $props();
</script>

<svelte:head>
	<link rel="icon" href="/favicon.ico" />
	<meta name="application-name" content="SABnzbd-Go" />
</svelte:head>

{@render children()}
