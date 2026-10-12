import { render, screen, fireEvent, within } from '@testing-library/svelte';
import { describe, it, expect, vi } from 'vitest';
import CategoriesSection from './CategoriesSection.svelte';

vi.mock('#lib/components/ui/separator/index.js', () => ({
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
			{ name: 'TV', dir: '/media/tv', pp: 2 },
			{ name: 'Movies', dir: '/media/movies', pp: 1 }
		]
	};

	// pp is cumulative: level 3 is repair + unpack + delete.
	it.each([
		['*', ['R', 'U', 'D']],
		['TV', ['R', 'U']],
		['Movies', ['R']]
	])('shows the stages level of %s implies', (name, badges) => {
		render(CategoriesSection, { configData: mockConfig, ...defaultCallbacks });
		const row = screen.getByText(name).closest('tr')!;
		const shown = ['R', 'U', 'D'].filter((b) => within(row).queryByText(b) !== null);
		expect(shown).toEqual(badges);
	});

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
