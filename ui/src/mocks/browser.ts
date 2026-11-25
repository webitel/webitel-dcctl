import { setupWorker } from 'msw/browser';

import { handlers } from './handlers';
import { selectVariantFromUrl } from './statusVariants';

export const worker = setupWorker(...handlers);

/**
 * Starts the mock. `?mock=normal|alarm|operation|unreachable|stale|unavailable|stretch`
 * in the URL picks the `GET /v1/status` and `GET /v1/datacenters/{dc}` variant for the whole page session.
 */
export const startMockWorker = () => {
	selectVariantFromUrl(window.location.search);
	return worker.start({
		onUnhandledRequest: 'warn',
		serviceWorker: { url: `${import.meta.env.BASE_URL}mockServiceWorker.js` },
	});
};
