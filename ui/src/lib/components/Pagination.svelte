<script lang="ts">
	import { Button } from '$lib/components/ui/button';

	let {
		total,
		limit,
		page,
		onPageChange
	}: {
		total: number;
		limit: number;
		page: number;
		onPageChange: (page: number) => void;
	} = $props();

	let totalPages = $derived(Math.ceil(total / limit));
	let start = $derived(page * limit + 1);
	let end = $derived(Math.min((page + 1) * limit, total));

	function next() {
		if (page < totalPages - 1) {
			onPageChange(page + 1);
		}
	}

	function prev() {
		if (page > 0) {
			onPageChange(page - 1);
		}
	}
</script>

{#if total > 0}
	<div class="mt-4 flex items-center justify-between px-2">
		<div class="text-sm text-gray-500">
			Showing <span class="font-medium text-gray-900 dark:text-gray-100">{start}</span>
			to <span class="font-medium text-gray-900 dark:text-gray-100">{end}</span>
			of <span class="font-medium text-gray-900 dark:text-gray-100">{total}</span> results
		</div>

		<div class="flex gap-2">
			<Button variant="outline" size="sm" onclick={prev} disabled={page === 0}>
				<svg
					xmlns="http://www.w3.org/2000/svg"
					viewBox="0 0 16 16"
					fill="currentColor"
					class="mr-1 size-4"
				>
					<path
						fill-rule="evenodd"
						d="M9.78 12.78a.75.75 0 0 1-1.06 0L4.47 8.53a.75.75 0 0 1 0-1.06l4.25-4.25a.75.75 0 0 1 1.06 1.06L6.06 8l3.72 3.72a.75.75 0 0 1 0 1.06Z"
						clip-rule="evenodd"
					/>
				</svg>
				Previous
			</Button>
			<Button variant="outline" size="sm" onclick={next} disabled={page >= totalPages - 1}>
				Next
				<svg
					xmlns="http://www.w3.org/2000/svg"
					viewBox="0 0 16 16"
					fill="currentColor"
					class="ml-1 size-4"
				>
					<path
						fill-rule="evenodd"
						d="M6.22 3.22a.75.75 0 0 1 1.06 0l4.25 4.25a.75.75 0 0 1 0 1.06l-4.25 4.25a.75.75 0 0 1-1.06-1.06L9.94 8 6.22 4.28a.75.75 0 0 1 0-1.06Z"
						clip-rule="evenodd"
					/>
				</svg>
			</Button>
		</div>
	</div>
{/if}
