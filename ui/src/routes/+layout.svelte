<script lang="ts">
	import '../app.css';
	import { onMount } from 'svelte';
	import { initTheme } from '$lib/stores/theme.svelte';

	let { children } = $props();

	onMount(() => initTheme());
</script>

<svelte:head>
	<meta name="application-name" content="GoNZBD" />
</svelte:head>

{@render children()}
