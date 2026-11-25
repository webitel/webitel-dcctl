import { createPinia, setActivePinia } from 'pinia';
import { beforeEach, describe, expect, it } from 'vitest';
import { createMemoryHistory } from 'vue-router';
import { AUTH_STORAGE_KEY, useAuthStore } from '@/stores/auth';

import {
	createAppRouter,
	redirectToLoginOnLogout,
	safeRedirect,
} from './index';

const signIn = () =>
	sessionStorage.setItem(
		AUTH_STORAGE_KEY,
		JSON.stringify({ name: 'support', token: 'dG9rZW4=' }),
	);

beforeEach(() => {
	setActivePinia(createPinia());
});

describe('scrollBehavior', () => {
	it('scrolls to the hash target', () => {
		const router = createAppRouter(createMemoryHistory('/dcctl/'));
		const scroll = router.options.scrollBehavior as (to: unknown) => unknown;

		expect(scroll({ hash: '#coordination' })).toEqual({ el: '#coordination' });
		expect(scroll({ hash: '' })).toBeUndefined();
	});
});

describe('safeRedirect', () => {
	it.each([
		['/operations', '/operations'],
		['/journal?x=1', '/journal?x=1'],
		[undefined, '/'],
		['', '/'],
		['https://evil.example/', '/'],
		['//evil.example/', '/'],
		['javascript:alert(1)', '/'],
		['/\\evil.example', '/'],
		[['/a'], '/'],
	])('%j -> %s', (input, expected) => {
		expect(safeRedirect(input as never)).toBe(expected);
	});
});

// The first lazy view import loads the whole SDK: slow on a busy machine.
describe('router', { timeout: 20_000 }, () => {
	it('sends an anonymous visitor to the login form and remembers the target', async () => {
		const router = createAppRouter(createMemoryHistory('/dcctl/'));

		await router.push('/operations');

		expect(router.currentRoute.value.name).toBe('login');
		expect(router.currentRoute.value.query.redirect).toBe('/operations');
	});

	it('lets a signed-in user through to every section', async () => {
		signIn();
		const router = createAppRouter(createMemoryHistory('/dcctl/'));

		for (const name of ['overview', 'datacenters', 'operations', 'journal']) {
			await router.push({ name });
			expect(router.currentRoute.value.name).toBe(name);
		}
	});

	it.each([
		['/datacenters/dc_b', 'datacenter', 'datacenters'],
		['/operations/0b9f', 'operation', 'operations'],
		['/', 'overview', 'overview'],
	])(
		'%s keeps the %s section active in the navigation',
		async (path, name, section) => {
			signIn();
			const router = createAppRouter(createMemoryHistory('/dcctl/'));

			await router.push(path);

			expect(router.currentRoute.value.name).toBe(name);
			expect(router.currentRoute.value.meta.section).toBe(section);
		},
	);

	it('does not show the login form to a signed-in user', async () => {
		signIn();
		const router = createAppRouter(createMemoryHistory('/dcctl/'));

		await router.push('/login?redirect=/journal');

		expect(router.currentRoute.value.name).toBe('journal');
	});

	it('serves the app under the /dcctl/ base', () => {
		const router = createAppRouter(createMemoryHistory('/dcctl/'));

		expect(router.resolve({ name: 'journal' }).href).toBe('/dcctl/journal');
		expect(createAppRouter().options.history.base).toBe('/dcctl');
	});

	it('opens the login form when the credentials are cleared', async () => {
		signIn();
		const router = createAppRouter(createMemoryHistory('/dcctl/'));
		redirectToLoginOnLogout(router);
		await router.push('/journal');

		useAuthStore().clear();
		await new Promise((resolve) => setTimeout(resolve));
		await router.isReady();

		expect(router.currentRoute.value.name).toBe('login');
		expect(router.currentRoute.value.query.redirect).toBe('/journal');
	});
});
