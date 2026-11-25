/**
 * nginx serves dcctl under `/dcctl/` and strips that prefix before the
 * proxy_pass, so the browser has to call `/dcctl/api/...`. A call to `/api`
 * would reach the main Webitel API on the same host.
 */
export const apiBaseUrl = (base: string): string => {
	const prefix = base.replace(/^\.\//, '/').replace(/\/+$/, '');
	return `${prefix}/api`;
};

export const API_BASE = apiBaseUrl(import.meta.env.BASE_URL);
