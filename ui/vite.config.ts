import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			compilerOptions: {
				runes: ({ filename }) => filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			},
			adapter: adapter({ pages: 'dist', assets: 'dist', fallback: 'index.html' }),
			prerender: { entries: [] }
		})
	],
	resolve: { conditions: ['browser', 'development'] },
	test: {
		fsModuleCache: true,
		include: ['src/**/*.{test,spec}.{js,ts}'],
		environment: 'happy-dom',
		globals: true,
		setupFiles: ['./src/setupTests.ts'],
		coverage: {
			provider: 'v8',
			reporter: ['text', 'html'],
			include: ['src/lib/**'],
			exclude: ['src/lib/components/ui/**', 'src/lib/types.ts']
		}
	},
	server: {
		proxy: {
			'/api': 'http://localhost:4289'
		}
	}
});
