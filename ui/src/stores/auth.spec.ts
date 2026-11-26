import { HttpResponse, http } from 'msw';
import { createPinia, setActivePinia } from 'pinia';
import {
	afterAll,
	afterEach,
	beforeAll,
	beforeEach,
	describe,
	expect,
	it,
} from 'vitest';

import { setupApiClient } from '@/api/client/setup';
import { getStatusService } from '@/api/gen/status-service/status-service';
import { MOCK_USER } from '@/mocks/handlers';
import { server } from '@/mocks/server';

import { AUTH_STORAGE_KEY, useAuthStore } from './auth';

beforeAll(() => {
	server.listen({ onUnhandledFrame: 'error' });
});
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
	setActivePinia(createPinia());
	setupApiClient();
});

describe('auth store', () => {
	it('starts logged out when nothing is stored', () => {
		const auth = useAuthStore();

		expect(auth.isAuthenticated).toBe(false);
		expect(auth.authorization).toBeNull();
		expect(auth.userName).toBe('');
	});

	it('restores the session of the tab from sessionStorage', () => {
		sessionStorage.setItem(
			AUTH_STORAGE_KEY,
			JSON.stringify({ name: 'support', token: 'QWxhZGRpbjpvcGVu' }),
		);
		const auth = useAuthStore();

		expect(auth.isAuthenticated).toBe(true);
		expect(auth.userName).toBe('support');
		expect(auth.authorization).toBe('Basic QWxhZGRpbjpvcGVu');
	});

	it.each([
		['not json', '{oops'],
		['wrong shape', JSON.stringify({ name: 1 })],
	])('ignores a corrupt stored session: %s', (_name, raw) => {
		sessionStorage.setItem(AUTH_STORAGE_KEY, raw);

		expect(useAuthStore().isAuthenticated).toBe(false);
	});

	it('logs in against the API and keeps the credentials in sessionStorage', async () => {
		const auth = useAuthStore();

		await auth.login(MOCK_USER.name, MOCK_USER.password);

		expect(auth.isAuthenticated).toBe(true);
		expect(auth.userName).toBe(MOCK_USER.name);
		const stored = sessionStorage.getItem(AUTH_STORAGE_KEY) ?? '';
		expect(stored).toContain(MOCK_USER.name);
		expect(localStorage.length).toBe(0);
	});

	it('sends the Authorization header on every request after login', async () => {
		const auth = useAuthStore();
		await auth.login(MOCK_USER.name, MOCK_USER.password);
		let seen: string | null = null;
		server.use(
			http.get('*/v1/status', ({ request }) => {
				seen = request.headers.get('Authorization');
				return HttpResponse.json({});
			}),
		);

		await getStatusService().getStatus();

		expect(seen).toBe(auth.authorization);
	});

	it('rejects a wrong password with 401 and stays logged out', async () => {
		const auth = useAuthStore();

		await expect(auth.login(MOCK_USER.name, 'wrong')).rejects.toMatchObject({
			response: { status: 401 },
		});

		expect(auth.isAuthenticated).toBe(false);
		expect(sessionStorage.getItem(AUTH_STORAGE_KEY)).toBeNull();
	});

	it('does not stay logged in when the login check fails for another reason', async () => {
		server.use(
			http.get('*/v1/status', () =>
				HttpResponse.json({ id: 'dcctl.unavailable' }, { status: 503 }),
			),
		);
		const auth = useAuthStore();

		await expect(
			auth.login(MOCK_USER.name, MOCK_USER.password),
		).rejects.toBeDefined();

		expect(auth.isAuthenticated).toBe(false);
		expect(sessionStorage.getItem(AUTH_STORAGE_KEY)).toBeNull();
	});

	it('logout clears the credentials', async () => {
		const auth = useAuthStore();
		await auth.login(MOCK_USER.name, MOCK_USER.password);

		auth.logout();

		expect(auth.isAuthenticated).toBe(false);
		expect(auth.authorization).toBeNull();
		expect(sessionStorage.getItem(AUTH_STORAGE_KEY)).toBeNull();
	});

	it('clears the credentials when any later request gets 401', async () => {
		const auth = useAuthStore();
		await auth.login(MOCK_USER.name, MOCK_USER.password);
		server.use(
			http.get('*/v1/status', () =>
				HttpResponse.json(
					{ id: 'dcctl.auth.required', code: 401 },
					{ status: 401 },
				),
			),
		);

		await expect(getStatusService().getStatus()).rejects.toBeDefined();

		expect(auth.isAuthenticated).toBe(false);
		expect(sessionStorage.getItem(AUTH_STORAGE_KEY)).toBeNull();
	});
});
