import { flushPromises, mount } from '@vue/test-utils';
import WebitelUI from '@webitel/ui-sdk';
import { eventBus } from '@webitel/ui-sdk/scripts';
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
	vi,
} from 'vitest';
import { createMemoryHistory } from 'vue-router';

import { setupApiClient } from '@/api/client/setup';
import { i18n } from '@/app/i18n';
import { messages } from '@/app/locale';
import { MOCK_USER } from '@/mocks/handlers';
import { server } from '@/mocks/server';
import { createAppRouter } from '@/router';
import { useAuthStore } from '@/stores/auth';

import LoginView from './LoginView.vue';

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const texts = messages.en.dcctl.login;

const mountLogin = async (redirect = '/journal') => {
	const pinia = createPinia();
	setActivePinia(pinia);
	setupApiClient();
	i18n.global.locale.value = 'en';
	const router = createAppRouter(createMemoryHistory('/dcctl/'));
	await router.push({ name: 'login', query: { redirect } });
	const wrapper = mount(LoginView, {
		global: {
			plugins: [pinia, i18n, router, [WebitelUI, { eventBus }]],
		},
		attachTo: document.body,
	});
	return { wrapper, router };
};

const fill = async (
	wrapper: Awaited<ReturnType<typeof mountLogin>>['wrapper'],
	name: string,
	password: string,
) => {
	const [nameInput, passwordInput] = wrapper.findAll('input');
	await nameInput.setValue(name);
	await passwordInput.setValue(password);
	await wrapper.find('form').trigger('submit');
	await flushPromises();
};

describe('LoginView', () => {
	beforeEach(() => {
		document.body.innerHTML = '';
	});

	it('shows the invalid-credentials message on 401 and clears the password', async () => {
		const { wrapper } = await mountLogin();

		await fill(wrapper, MOCK_USER.name, 'wrong');

		expect(wrapper.text()).toContain(texts.invalid);
		expect(wrapper.findAll('input')[1].element.value).toBe('');
		expect(useAuthStore().isAuthenticated).toBe(false);
		wrapper.unmount();
	});

	it('shows a failure and a notification with detail and id on another error', async () => {
		server.use(
			http.get('*/v1/status', () =>
				HttpResponse.json(
					{ id: 'dcctl.unavailable', code: 503, detail: 'arbiter is down' },
					{ status: 503 },
				),
			),
		);
		const notifications: unknown[] = [];
		const onNotification = (payload: unknown) => notifications.push(payload);
		eventBus.$on('notification', onNotification);
		const { wrapper } = await mountLogin();

		await fill(wrapper, MOCK_USER.name, MOCK_USER.password);

		expect(wrapper.text()).toContain(texts.failed);
		expect(notifications).toEqual([
			{ type: 'error', text: 'arbiter is down (dcctl.unavailable)' },
		]);
		eventBus.$off('notification', onNotification);
		wrapper.unmount();
	});

	it('redirects to the requested page after a successful login', async () => {
		const { wrapper, router } = await mountLogin('/journal');
		const replace = vi.spyOn(router, 'replace');

		await fill(wrapper, MOCK_USER.name, MOCK_USER.password);

		expect(replace).toHaveBeenCalledWith('/journal');
		expect(useAuthStore().isAuthenticated).toBe(true);
		wrapper.unmount();
	});
});
