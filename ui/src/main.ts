import '@webitel/ui-sdk/dist/ui-sdk.css';

import WebitelUI from '@webitel/ui-sdk';
import { eventBus, reloadOnStaleChunk } from '@webitel/ui-sdk/scripts';
import { createPinia } from 'pinia';
import { createApp } from 'vue';

import { setupApiClient } from '@/api/client/setup';
import { i18n } from '@/app/i18n';
import { createAppRouter, redirectToLoginOnLogout } from '@/router';

import App from './App.vue';

const bootstrap = async () => {
	reloadOnStaleChunk();

	// `npm run dev` runs on the mock; `npm run dev:backend` (mode backend) talks to a real API.
	if (import.meta.env.DEV && import.meta.env.MODE !== 'backend') {
		const { startMockWorker } = await import('@/mocks/browser');
		await startMockWorker();
	}

	const app = createApp(App);
	const pinia = createPinia();
	const router = createAppRouter();

	app.use(pinia);
	app.use(i18n);
	app.use(WebitelUI, { eventBus });
	setupApiClient();
	redirectToLoginOnLogout(router);
	app.use(router);

	app.mount('#app');
};

bootstrap();
