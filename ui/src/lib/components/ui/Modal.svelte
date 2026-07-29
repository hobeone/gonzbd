<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		open = $bindable(false),
		ariaLabel,
		children,
		class: className = ''
	}: {
		open?: boolean;
		ariaLabel?: string;
		children?: Snippet;
		class?: string;
	} = $props();

	let dialogEl: HTMLDialogElement | undefined = $state();

	$effect(() => {
		if (open && dialogEl && !dialogEl.open) {
			dialogEl.showModal();
		}
	});

	function handleClose() {
		open = false;
	}

	function handleBackdropClick(e: MouseEvent) {
		if (e.target === dialogEl) {
			open = false;
		}
	}
</script>

{#if open}
	<dialog
		bind:this={dialogEl}
		aria-label={ariaLabel}
		onclose={handleClose}
		onclick={handleBackdropClick}
		class="backdrop:bg-black/75 backdrop:backdrop-blur-md bg-white dark:bg-[#111827] text-foreground p-0 outline-none m-auto shadow-2xl rounded-2xl border border-border dark:border-slate-700/80 ring-1 ring-black/5 dark:ring-white/10 opacity-100 {className}"
	>
		{@render children?.()}
	</dialog>
{/if}
