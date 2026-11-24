import axios, {
	type AxiosAdapter,
	AxiosError,
	type InternalAxiosRequestConfig,
} from 'axios';
import { describe, expect, it, vi } from 'vitest';

import { installAuthInterceptors } from './interceptors';

const respondWith =
	(status: number, body: unknown = {}): AxiosAdapter =>
	async (config: InternalAxiosRequestConfig) => {
		const response = {
			data: body,
			status,
			statusText: '',
			headers: {},
			config,
		};
		if (status >= 400) {
			throw new AxiosError(
				'failed',
				AxiosError.ERR_BAD_REQUEST,
				config,
				null,
				response,
			);
		}
		return response;
	};

const setup = (status: number, authorization: string | null) => {
	const hooks = {
		getAuthorization: vi.fn(() => authorization),
		onUnauthorized: vi.fn(),
	};
	const instance = axios.create({ adapter: respondWith(status) });
	installAuthInterceptors(instance, hooks);
	return { instance, hooks };
};

describe('installAuthInterceptors', () => {
	it('sends the Authorization header on every request', async () => {
		let seen: unknown;
		const instance = axios.create({
			adapter: async (config) => {
				seen = config.headers.get('Authorization');
				return { data: {}, status: 200, statusText: '', headers: {}, config };
			},
		});
		installAuthInterceptors(instance, {
			getAuthorization: () => 'Basic abc',
			onUnauthorized: vi.fn(),
		});

		await instance.get('/v1/status');

		expect(seen).toBe('Basic abc');
	});

	it('sends no Authorization header without credentials', async () => {
		let seen: unknown = 'unset';
		const instance = axios.create({
			adapter: async (config) => {
				seen = config.headers.get('Authorization');
				return { data: {}, status: 200, statusText: '', headers: {}, config };
			},
		});
		installAuthInterceptors(instance, {
			getAuthorization: () => null,
			onUnauthorized: vi.fn(),
		});

		await instance.get('/v1/status');

		expect(seen).toBeUndefined();
	});

	it('calls onUnauthorized on 401 and still rejects the request', async () => {
		const { instance, hooks } = setup(401, 'Basic abc');

		await expect(instance.get('/v1/status')).rejects.toMatchObject({
			response: { status: 401 },
		});
		expect(hooks.onUnauthorized).toHaveBeenCalledTimes(1);
	});

	it.each([400, 403, 404, 409, 503])('ignores %s', async (status) => {
		const { instance, hooks } = setup(status, 'Basic abc');

		await expect(instance.get('/v1/status')).rejects.toBeDefined();
		expect(hooks.onUnauthorized).not.toHaveBeenCalled();
	});

	it('ignores a late 401 of a request sent before the credentials changed', async () => {
		let current = 'Basic old';
		const hooks = {
			getAuthorization: () => current,
			onUnauthorized: vi.fn(),
		};
		const instance = axios.create({
			adapter: async (config) => {
				current = 'Basic new-session'; // user re-logged in while the request was in flight
				return respondWith(401)(config);
			},
		});
		installAuthInterceptors(instance, hooks);

		await expect(instance.get('/v1/status')).rejects.toBeDefined();

		expect(hooks.onUnauthorized).not.toHaveBeenCalled();
	});
});
