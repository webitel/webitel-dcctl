import type { AxiosRequestConfig } from 'axios';

import { apiClient } from './apiClient';

/**
 * Orval mutator: every generated call goes through the shared axios instance.
 * `options` (timeout, abort signal, ...) are merged over the generated config.
 */
export const dcctlAxios = <T>(
	config: AxiosRequestConfig,
	options?: AxiosRequestConfig,
): Promise<T> =>
	apiClient
		.request<T>({ ...config, ...options })
		.then((response) => response.data);
