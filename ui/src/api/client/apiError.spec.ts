import { AxiosError } from 'axios';
import { describe, expect, it } from 'vitest';

import { apiErrorMessage, getApiError, isUnauthorized } from './apiError';

const axiosError = (data: unknown, status = 409) =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		data,
		status,
		statusText: '',
		headers: {},
		config: {} as never,
	});

describe('getApiError', () => {
	it('returns the error body of an API response', () => {
		const body = {
			id: 'dcctl.operation.running',
			code: 409,
			detail: 'another operation is running',
		};

		expect(getApiError(axiosError(body))).toEqual(body);
	});

	it.each([
		['a plain Error', new Error('boom')],
		['a response without id', axiosError({ message: 'x' })],
		['a non-object body', axiosError('<html>')],
		['a network error', new AxiosError('Network Error')],
	])('returns null for %s', (_name, error) => {
		expect(getApiError(error)).toBeNull();
	});
});

describe('apiErrorMessage', () => {
	it('shows detail together with id', () => {
		expect(
			apiErrorMessage(
				axiosError({ id: 'dcctl.operation.running', detail: 'busy' }),
			),
		).toBe('busy (dcctl.operation.running)');
	});

	it('shows the id alone when detail is empty', () => {
		expect(apiErrorMessage(axiosError({ id: 'dcctl.x' }))).toBe('dcctl.x');
	});

	it('falls back to the error message for non-API errors', () => {
		expect(apiErrorMessage(new Error('Network Error'))).toBe('Network Error');
	});
});

describe('isUnauthorized', () => {
	it('is true for a 401 response only', () => {
		expect(isUnauthorized(axiosError({}, 401))).toBe(true);
		expect(isUnauthorized(axiosError({}, 409))).toBe(false);
		expect(isUnauthorized(new Error('x'))).toBe(false);
		expect(isUnauthorized(undefined)).toBe(false);
	});
});
