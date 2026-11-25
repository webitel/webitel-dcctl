import { AxiosError, type AxiosResponse } from 'axios';

import type { ApiError } from '@/api/client/apiError';

/** A failed request the API answered with an `Error` body, as axios reports it. */
export const apiFailure = (body: ApiError, status = body.code ?? 400) =>
	new AxiosError(body.detail ?? body.id, String(status), undefined, undefined, {
		status,
		data: body,
	} as AxiosResponse);
