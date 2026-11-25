import { useAuthStore } from '@/stores/auth';

import { apiClient } from './apiClient';
import { installAuthInterceptors } from './interceptors';
import { installServerClock } from './serverClock';

let installed = false;

/** Wires the shared axios instance to the auth store. Call once, after pinia is active. */
export const setupApiClient = (): void => {
	if (installed) return;
	installed = true;
	installServerClock(apiClient);
	installAuthInterceptors(apiClient, {
		getAuthorization: () => useAuthStore().authorization,
		onUnauthorized: () => useAuthStore().clear(),
	});
};
