import { mount } from '@vue/test-utils';
import WebitelUI from '@webitel/ui-sdk';
import { eventBus } from '@webitel/ui-sdk/scripts';
import { createPinia, setActivePinia } from 'pinia';
import type { Component } from 'vue';
import { createMemoryHistory } from 'vue-router';

import { setupApiClient } from '@/api/client/setup';
import { i18n } from '@/app/i18n';
import { createAppRouter } from '@/router';

/** Mounts a component with pinia, i18n (en), the router and the SDK, as the app does. */
export const mountWithApp = async (
	component: Component,
	props: Record<string, unknown> = {},
	token = 'dG9rZW4=',
) => {
	const pinia = createPinia();
	setActivePinia(pinia);
	setupApiClient();
	i18n.global.locale.value = 'en';
	const router = createAppRouter(createMemoryHistory('/dcctl/'));
	sessionStorage.setItem(
		'dcctl.auth',
		JSON.stringify({ name: 'support', token }),
	);
	await router.push('/');
	await router.isReady();
	const wrapper = mount(component, {
		props,
		global: { plugins: [pinia, i18n, router, [WebitelUI, { eventBus }]] },
		attachTo: document.body,
	});
	return { wrapper, router, pinia };
};
