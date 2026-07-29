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
		class="backdrop:bg-black/60 backdrop:backdrop-blur-sm bg-transparent p-0 outline-none m-auto shadow-2xl rounded-2xl border-0 {className}"
	>
		{@render children?.()}
	</dialog>
{/if}
