import { render, screen, fireEvent } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import CategoriesSection from './CategoriesSection.svelte';

vi.mock('$lib/components/ui/separator', () => ({
	Separator: function MockSeparator() {}
}));

describe('CategoriesSection', () => {
	const defaultCallbacks = {
		onAddCategory: vi.fn(),
		onEditCategory: vi.fn(),
		onDeleteCategory: vi.fn()
	};

	const mockConfig = {
		categories: [
			{ name: '*', dir: '', pp: 3 },
			{ name: 'TV', dir: '/media/tv', pp: 7 },
			{ name: 'Movies', dir: '/media/movies', pp: 1 }
		]
	};

	it('renders the heading', () => {
		render(CategoriesSection, { configData: mockConfig, ...defaultCallbacks });
		expect(screen.getByText('Categories')).toBeInTheDocument();
	});

	it('renders category names', () => {
		render(CategoriesSection, { configData: mockConfig, ...defaultCallbacks });
		expect(screen.getByText('TV')).toBeInTheDocument();
		expect(screen.getByText('Movies')).toBeInTheDocument();
	});

	it('shows (default) for categories with empty dir', () => {
		render(CategoriesSection, { configData: mockConfig, ...defaultCallbacks });
		expect(screen.getByText('(default)')).toBeInTheDocument();
	});

	it('calls onAddCategory when Add button clicked', async () => {
		const onAddCategory = vi.fn();
		render(CategoriesSection, { configData: mockConfig, ...defaultCallbacks, onAddCategory });
		await fireEvent.click(screen.getByText('+ Add Category'));
		expect(onAddCategory).toHaveBeenCalled();
	});
});
