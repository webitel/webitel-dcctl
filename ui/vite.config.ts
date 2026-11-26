import { fileURLToPath, URL } from 'node:url';
import vue from '@vitejs/plugin-vue';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';
import { precompress } from './precompress';

// The UI is served by the dcctl binary under /dcctl/ (see router base).
const BASE = '/dcctl/';

export default defineConfig(({ mode, command }) => {
	const env = loadEnv(mode, process.cwd(), '');
	// `npm run dev:backend` proxies /dcctl/api to a running dcctl, cutting the /dcctl prefix
	// as nginx does; the default dev mode runs on the mock.
	const apiProxy =
		mode === 'backend'
			? (env.VITE_API_PROXY ?? 'http://127.0.0.1:10022')
			: undefined;

	return {
		base: BASE,
		// public/ holds only the msw worker, which must not reach the production bundle.
		publicDir: command === 'build' ? false : 'public',
		plugins: [vue(), precompress()],
		resolve: {
			alias: {
				'@': fileURLToPath(new URL('./src', import.meta.url)),
			},
		},
		server: {
			fs: { allow: ['..'] },
			proxy: apiProxy
				? {
						[`${BASE}api`]: {
							target: apiProxy,
							changeOrigin: true,
							rewrite: (path) => path.slice(BASE.length - 1),
						},
					}
				: undefined,
		},
		test: {
			environment: 'jsdom',
			globals: false,
			// Vitest serves from `/`; the app is served under BASE, and the API base is derived from it.
			env: { BASE_URL: BASE },
			include: ['src/**/*.spec.ts', '*.spec.ts'],
			setupFiles: ['src/tests/setup.ts'],
			// The first test of a file waits for the SDK to load its components lazily.
			testTimeout: 20_000,
			server: { deps: { inline: [/@webitel\//, /primevue/] } },
		},
	};
});
