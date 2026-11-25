import { fileURLToPath, URL } from 'node:url';
import vue from '@vitejs/plugin-vue';
import { loadEnv } from 'vite';
import { defineConfig } from 'vitest/config';

// The UI is served by the dcctl binary under /dcctl/ (see router base).
const BASE = '/dcctl/';

export default defineConfig(({ mode, command }) => {
	const env = loadEnv(mode, process.cwd(), '');
	// `npm run dev:backend` proxies /api to a running dcctl; the default dev mode runs on the mock.
	const apiProxy =
		mode === 'backend'
			? (env.VITE_API_PROXY ?? 'http://127.0.0.1:10022')
			: undefined;

	return {
		base: BASE,
		// public/ holds only the msw worker, which must not reach the production bundle.
		publicDir: command === 'build' ? false : 'public',
		plugins: [vue()],
		resolve: {
			alias: {
				'@': fileURLToPath(new URL('./src', import.meta.url)),
			},
		},
		server: {
			fs: { allow: ['..'] },
			proxy: apiProxy
				? { '/api': { target: apiProxy, changeOrigin: true } }
				: undefined,
		},
		test: {
			environment: 'jsdom',
			globals: false,
			include: ['src/**/*.spec.ts'],
			setupFiles: ['src/tests/setup.ts'],
			server: { deps: { inline: [/@webitel\//, /primevue/] } },
		},
	};
});
