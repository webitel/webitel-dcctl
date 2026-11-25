import { setupWorker } from 'msw/browser';

import { handlers } from './handlers';
import { selectOperationsVariantFromUrl } from './operationVariants';
import { selectVariantFromUrl } from './statusVariants';

export const worker = setupWorker(...handlers);

/**
 * Starts the mock. `?mock=normal|alarm|operation|unreachable|stale|unavailable|stretch`
 * in the URL picks the `GET /v1/status` and `GET /v1/datacenters/{dc}` variant for the whole page session.
 * `?ops=normal|live|many|empty|unavailable` picks the variant of the operations list. The operations
 * `watch-progress` and `watch-drop` move on their own; the second breaks its stream once.
 */
export const startMockWorker = () => {
	selectVariantFromUrl(window.location.search);
	selectOperationsVariantFromUrl(window.location.search);
	return worker.start({
		onUnhandledRequest: 'warn',
		serviceWorker: { url: `${import.meta.env.BASE_URL}mockServiceWorker.js` },
	});
};
