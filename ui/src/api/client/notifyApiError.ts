import { eventBus } from '@webitel/ui-sdk/scripts';

import { apiErrorMessage, isUnauthorized } from './apiError';

/**
 * Shows `detail (id)` of a failed request in the SDK notification bar.
 * Does nothing for `401`: that opens the login form instead.
 */
export const notifyApiError = (error: unknown): void => {
	if (isUnauthorized(error)) return;
	eventBus.$emit('notification', {
		type: 'error',
		text: apiErrorMessage(error),
	});
};
