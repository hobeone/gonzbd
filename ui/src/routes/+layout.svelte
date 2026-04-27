<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';

	let { children } = $props();

	onMount(() => {
		if (window.matchMedia('(prefers-color-scheme: dark)').matches) {
			document.documentElement.classList.add('dark');
		}
	});
</script>

<svelte:head>
	<link rel="icon" href="/favicon.ico" />
	<meta name="application-name" content="SABnzbd-Go" />
</svelte:head>

{@render children()}
