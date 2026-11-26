import { flushPromises } from '@vue/test-utils';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';

import { MOCK_USER } from '@/mocks/handlers';
import { server } from '@/mocks/server';
import { setStatusVariant } from '@/mocks/statusVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import { waitForLazy } from '@/tests/waitForLazy';
import { encodeBasicToken } from '@/utils/basicAuth';
import AppShell from './AppShell.vue';

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterEach(() => {
	server.resetHandlers();
	setStatusVariant('normal');
});
afterAll(() => server.close());

const mountShell = async () => {
	return mountWithApp(
		AppShell,
		{},
		encodeBasicToken(MOCK_USER.name, MOCK_USER.password),
	);
};

describe('AppShell', () => {
	it('loads the status into the header and the overview', async () => {
		setStatusVariant('alarm');
		const { wrapper } = await mountShell();

		await waitForLazy(() => {
			expect(wrapper.text()).toContain('View from dc_a');
			expect(wrapper.text()).toContain('Database split');
		});
		wrapper.unmount();
	});

	it('shows the operation line on every page while an operation runs', async () => {
		setStatusVariant('operation');
		const { wrapper, router } = await mountShell();
		await router.push('/journal');

		await waitForLazy(() =>
			expect(wrapper.find('[data-test="operation-line"]').exists()).toBe(true),
		);
		wrapper.unmount();
	});

	it('stops polling when it is unmounted', async () => {
		const { wrapper, pinia } = await mountShell();
		const store = useStatusStore(pinia);
		await waitForLazy(() => expect(store.status).toBeDefined());

		wrapper.unmount();
		await flushPromises();

		expect(store.status).toBeUndefined();
	});
});
