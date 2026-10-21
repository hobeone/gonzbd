import { render } from '@testing-library/svelte';
import { describe, it, expect } from 'vitest';
import SpeedGraph from './SpeedGraph.svelte';

describe('SpeedGraph', () => {
	it('renders an SVG element', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [10, 20, 30], width: 120, height: 32 }
		});
		const svg = container.querySelector('svg');
		expect(svg).toBeInTheDocument();
	});

	it('renders polyline when data has 2+ points', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [10, 20, 30], width: 120, height: 32 }
		});
		const polyline = container.querySelector('polyline');
		expect(polyline).toBeInTheDocument();
		expect(polyline?.getAttribute('points')).toBeTruthy();
	});

	it('does not render polyline for fewer than 2 data points', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [10], width: 120, height: 32 }
		});
		expect(container.querySelector('polyline')).not.toBeInTheDocument();
	});

	it('does not render polyline for empty data', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [], width: 120, height: 32 }
		});
		expect(container.querySelector('polyline')).not.toBeInTheDocument();
	});

	it('sets SVG width and height attributes', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [1, 2], width: 200, height: 50 }
		});
		const svg = container.querySelector('svg');
		expect(svg?.getAttribute('width')).toBe('200');
		expect(svg?.getAttribute('height')).toBe('50');
	});

	it('uses default dimensions when not specified', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [1, 2] }
		});
		const svg = container.querySelector('svg');
		expect(svg?.getAttribute('width')).toBe('120');
		expect(svg?.getAttribute('height')).toBe('32');
	});

	it('polyline points scale correctly', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [0, 100], width: 100, height: 100 }
		});
		const polyline = container.querySelector('polyline');
		expect(polyline?.getAttribute('points')).toBe('0,100 100,2');
	});

	it('scales multi-point series including intermediate values', () => {
		const { container } = render(SpeedGraph, {
			props: { data: [0, 50, 100], width: 100, height: 100 }
		});
		const polyline = container.querySelector('polyline');
		expect(polyline?.getAttribute('points')).toBe('0,100 50,51 100,2');
	});
});
