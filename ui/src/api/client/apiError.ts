import { isAxiosError } from 'axios';

import type { WebitelDcctlV1Error } from '@/api/gen/_models';

export type ApiError = WebitelDcctlV1Error;

/** The API error body of a failed request, or `null` for anything else. Errors are told apart by `id`, not by HTTP status. */
export const getApiError = (error: unknown): ApiError | null => {
	if (!isAxiosError(error)) return null;
	const body: unknown = error.response?.data;
	if (typeof body !== 'object' || body === null) return null;
	return typeof (body as ApiError).id === 'string' ? (body as ApiError) : null;
};

/** `true` for a failed request that the API answered with 401. */
export const isUnauthorized = (error: unknown): boolean =>
	isAxiosError(error) && error.response?.status === 401;

/** `detail (id)` for notifications. */
export const apiErrorMessage = (error: unknown): string => {
	const apiError = getApiError(error);
	if (apiError) {
		return apiError.detail
			? `${apiError.detail} (${apiError.id})`
			: (apiError.id as string);
	}
	return error instanceof Error ? error.message : String(error);
};
