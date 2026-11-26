import { setupWorker } from 'msw/browser';

import { selectActionVariantFromUrl } from './actionVariants';
import { handlers } from './handlers';
import { selectJournalVariantFromUrl } from './journalVariants';
import { selectOperationsVariantFromUrl } from './operationVariants';
import { selectVariantFromUrl } from './statusVariants';

export const worker = setupWorker(...handlers);

/**
 * Starts the mock. `?mock=normal|alarm|operation|unreachable|stale|unavailable|stretch`
 * in the URL picks the `GET /v1/status` and `GET /v1/datacenters/{dc}` variant for the whole page session.
 * `?ops=normal|live|many|empty|unavailable` picks the variant of the operations list. The operations
 * `watch-progress` and `watch-drop` move on their own; the second breaks its stream once.
 * `?journal=normal|many|empty|filter-error|unavailable` picks the variant of the journal: `filter-error`
 * rejects every filter with `dcctl.request.invalid`, `many` has 150 entries for paging.
 * `?action=ok|confirmation|confirmation-grew|preflight-failed|lock-busy|lock-busy-grew|preflight-failed-real|not-interrupted|operation-running|unavailable|invalid`
 * picks how the action endpoints answer: `confirmation` asks for confirmations, `confirmation-grew` finds a larger
 * amount on the real call, `lock-busy` refuses only the real call.
 */
export const startMockWorker = () => {
	selectVariantFromUrl(window.location.search);
	selectOperationsVariantFromUrl(window.location.search);
	selectJournalVariantFromUrl(window.location.search);
	selectActionVariantFromUrl(window.location.search);
	return worker.start({
		onUnhandledFrame: 'warn',
		serviceWorker: { url: `${import.meta.env.BASE_URL}mockServiceWorker.js` },
	});
};
