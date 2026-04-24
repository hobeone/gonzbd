import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import Pagination from './Pagination.svelte';

describe('Pagination', () => {
	it('shows correct range text', () => {
		render(Pagination, { total: 25, limit: 10, page: 0, onPageChange: vi.fn() });
		expect(screen.getByText('1')).toBeInTheDocument();
		expect(screen.getByText('10')).toBeInTheDocument();
		expect(screen.getByText('25')).toBeInTheDocument();
	});

	it('previous button disabled on first page', () => {
		render(Pagination, { total: 25, limit: 10, page: 0, onPageChange: vi.fn() });
		const prevBtn = screen.getByText('Previous').closest('button');
		expect(prevBtn).toBeDisabled();
	});

	it('next button disabled on last page', () => {
		render(Pagination, { total: 25, limit: 10, page: 2, onPageChange: vi.fn() });
		const nextBtn = screen.getByText('Next').closest('button');
		expect(nextBtn).toBeDisabled();
	});

	it('clicking next calls onPageChange with page + 1', async () => {
		const onPageChange = vi.fn();
		render(Pagination, { total: 25, limit: 10, page: 0, onPageChange });

		const nextBtn = screen.getByText('Next').closest('button')!;
		await fireEvent.click(nextBtn);

		expect(onPageChange).toHaveBeenCalledWith(1);
	});

	it('clicking previous calls onPageChange with page - 1', async () => {
		const onPageChange = vi.fn();
		render(Pagination, { total: 25, limit: 10, page: 1, onPageChange });

		const prevBtn = screen.getByText('Previous').closest('button')!;
		await fireEvent.click(prevBtn);

		expect(onPageChange).toHaveBeenCalledWith(0);
	});

	it('is hidden when total is 0', () => {
		const { container } = render(Pagination, { total: 0, limit: 10, page: 0, onPageChange: vi.fn() });
		expect(container.querySelector('.mt-4')).toBeNull();
	});
});
