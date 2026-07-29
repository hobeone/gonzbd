<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		open = $bindable(false),
		children,
		class: className = ''
	}: {
		open?: boolean;
		children?: Snippet;
		class?: string;
	} = $props();

	let dialogEl: HTMLDialogElement | undefined = $state();

	$effect(() => {
		if (!dialogEl) return;
		if (open && !dialogEl.open) {
			dialogEl.showModal();
		} else if (!open && dialogEl.open) {
			dialogEl.close();
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

<dialog
	bind:this={dialogEl}
	onclose={handleClose}
	onclick={handleBackdropClick}
	class="backdrop:bg-black/60 backdrop:backdrop-blur-sm bg-transparent p-0 outline-none max-w-none max-h-none m-auto shadow-2xl rounded-2xl border-0 {className}"
>
	{#if open}
		{@render children?.()}
	{/if}
</dialog>
