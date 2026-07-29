<script lang="ts">
	import Modal from '$lib/components/ui/Modal.svelte';
	import { getShortcuts, type ShortcutDef } from '$lib/shortcuts.svelte';
	import Keyboard from '@lucide/svelte/icons/keyboard';

	let { open = $bindable(false) }: { open?: boolean } = $props();

	let shortcuts = $derived(getShortcuts());

	function formatKey(def: ShortcutDef): string {
		const parts: string[] = [];

		if (def.mod === 'ctrl') {
			const isMac =
				typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent);
			parts.push(isMac ? '⌘' : 'Ctrl');
		} else if (def.mod === 'alt') {
			const isMac =
				typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.userAgent);
			parts.push(isMac ? '⌥' : 'Alt');
		} else if (def.mod === 'shift') {
			parts.push('Shift');
		}

		const keyLabels: Record<string, string> = {
			' ': 'Space',
			Escape: 'Esc',
			ArrowUp: '↑',
			ArrowDown: '↓',
			ArrowLeft: '←',
			ArrowRight: '→',
			Delete: 'Del',
			Backspace: '⌫'
		};
		parts.push(keyLabels[def.key] ?? def.key.toUpperCase());

		return parts.join(' + ');
	}
</script>

<Modal bind:open ariaLabel="Keyboard Shortcuts" class="w-full max-w-sm">
	<div class="border-b border-border/60 px-6 py-4">
		<h2 class="flex items-center gap-2.5 text-base font-bold tracking-tight text-foreground">
			<Keyboard class="size-5 text-primary" />
			Keyboard Shortcuts
		</h2>
	</div>

	<div class="px-6 py-4">
		<table class="w-full">
			<tbody>
				{#each shortcuts as shortcut (shortcut.key + (shortcut.mod ?? ''))}
					<tr class="group">
						<td class="py-1.5 pr-4">
							<kbd
								class="inline-flex min-w-[2rem] items-center justify-center rounded-lg border border-border bg-muted/60 px-2 py-0.5 font-mono text-xs font-semibold text-foreground"
							>
								{formatKey(shortcut)}
							</kbd>
						</td>
						<td
							class="py-1.5 text-sm text-muted-foreground"
						>
							{shortcut.description}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>

	<div
		class="border-t border-border/60 px-6 py-4 text-center text-xs text-muted-foreground font-medium"
	>
		Press <kbd
			class="rounded-md border border-border bg-muted/60 px-1.5 py-0.5 font-mono text-[10px]"
			>?</kbd
		>
		or
		<kbd
			class="rounded-md border border-border bg-muted/60 px-1.5 py-0.5 font-mono text-[10px]"
			>Esc</kbd
		> to close
	</div>
</Modal>
