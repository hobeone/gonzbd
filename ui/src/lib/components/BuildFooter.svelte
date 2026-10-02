<script lang="ts">
	import { onMount } from 'svelte';
	import { fetchBuildInfo, type BuildInfoResponse } from '#lib/api.js';
	import { formatBuildLabel, formatTimestamp } from '#lib/utils.js';

	let info = $state<BuildInfoResponse | null>(null);

	// A failed fetch leaves the footer empty: build info is informational
	// and the About dialog reports its own errors.
	onMount(() => {
		fetchBuildInfo()
			.then((res) => {
				info = res;
			})
			.catch(() => {});
	});

	const label = $derived(info ? formatBuildLabel(info.version, info.commit, info.dirty) : '');
	const tooltip = $derived.by(() => {
		if (!info) return '';
		const committed = formatTimestamp(info.commit_time);
		const built = formatTimestamp(info.build_date);
		return [committed && `Committed ${committed}`, built && `Built ${built}`]
			.filter(Boolean)
			.join('\n');
	});
</script>

{#if info}
	<footer
		data-testid="build-footer"
		class="mx-auto w-full max-w-7xl px-4 pb-4 text-center font-mono text-[11px] text-muted-foreground/70"
	>
		<span title={tooltip || undefined}>{label}</span>
	</footer>
{/if}
