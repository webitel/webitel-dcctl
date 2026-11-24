import type { AxiosRequestConfig } from 'axios';

import { apiClient } from './apiClient';

/** Orval mutator: every generated call goes through the shared axios instance. */
export const dcctlAxios = <T>(config: AxiosRequestConfig): Promise<T> =>
	apiClient.request<T>(config).then((response) => response.data);
