import { describe, expect, it } from 'vitest';

import { API_BASE, apiBaseUrl } from './apiBase';
import { apiClient } from './apiClient';

describe('apiBaseUrl', () => {
	it.each([
		['/dcctl/', '/dcctl/api'],
		['/dcctl', '/dcctl/api'],
		['/', '/api'],
		['./', '/api'],
	])('%s -> %s', (base, expected) => {
		expect(apiBaseUrl(base)).toBe(expected);
	});
});

describe('API base of the app', () => {
	it('sits under the prefix nginx strips, not at the site root', () => {
		expect(API_BASE).toBe('/dcctl/api');
	});

	it('is the base of the shared axios instance', async () => {
		let requested = '';
		await apiClient.get('/v1/status', {
			adapter: async (config) => {
				requested = `${config.baseURL}${config.url}`;
				return { data: {}, status: 200, statusText: '', headers: {}, config };
			},
		});
		expect(requested).toBe('/dcctl/api/v1/status');
	});
});
