import type { AxiosInstance } from 'axios';

export interface AuthInterceptorHooks {
	/** Current `Authorization` header value, or `null` when logged out. */
	getAuthorization: () => string | null;
	/** Called on `401` for a request sent with the current credentials. */
	onUnauthorized: () => void;
}

/**
 * Adds the `Authorization` header to every request and reports `401`.
 * The API sends `401` without `WWW-Authenticate`, so the browser shows no
 * native login dialog and the app has to open its own form.
 */
export const installAuthInterceptors = (
	instance: AxiosInstance,
	{ getAuthorization, onUnauthorized }: AuthInterceptorHooks,
): void => {
	instance.interceptors.request.use((config) => {
		const authorization = getAuthorization();
		if (authorization) config.headers.set('Authorization', authorization);
		return config;
	});

	instance.interceptors.response.use(undefined, (error) => {
		const sent = error?.config?.headers?.get?.('Authorization') ?? null;
		// A late 401 of a request from a previous session must not log out the new one.
		if (error?.response?.status === 401 && sent === getAuthorization()) {
			onUnauthorized();
		}
		return Promise.reject(error);
	});
};
