import { flushPromises } from '@vue/test-utils';
import { AxiosError } from 'axios';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { buildStatus } from '@/mocks/statusVariants';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import OverviewView from './OverviewView.vue';

const scrollIntoView = vi.fn();

beforeEach(() => {
	scrollIntoView.mockReset();
	Element.prototype.scrollIntoView = scrollIntoView;
});

const mountOverview = async () => {
	const mounted = await mountWithApp(OverviewView);
	return { ...mounted, store: useStatusStore(mounted.pinia) };
};

describe('OverviewView without data', () => {
	it('does not look like an all-clear while the first request is running', async () => {
		const { wrapper } = await mountOverview();

		const text = wrapper.text();
		expect(text).not.toContain('No alarms');
		expect(text).not.toContain('No datacenters');
		expect(text).not.toContain('no data');
		expect(text).toContain('Loading');
	});

	it('shows why there is no status instead of empty blocks when the API is down', async () => {
		const { wrapper, store } = await mountOverview();
		store.error = new AxiosError('Network Error');
		await flushPromises();

		const text = wrapper.text();
		expect(text).toContain('Cannot get the status');
		expect(text).toContain('Network Error');
		expect(text).not.toContain('No alarms');
		expect(text).not.toContain('No datacenters');
		expect(wrapper.find('#coordination').exists()).toBe(false);
	});
});

describe('OverviewView with data', () => {
	it('shows alarms, datacenters and coordination', async () => {
		const { wrapper, store } = await mountOverview();
		store.status = buildStatus('alarm');
		await flushPromises();

		expect(wrapper.find('[data-test="alarm"]').exists()).toBe(true);
		expect(wrapper.find('#datacenters').exists()).toBe(true);
		expect(wrapper.find('#coordination').exists()).toBe(true);
		expect(wrapper.text()).not.toContain('Cannot get the status');
	});

	it('keeps the blocks on screen when a later poll fails', async () => {
		const { wrapper, store } = await mountOverview();
		store.status = buildStatus('normal');
		store.error = new AxiosError('Network Error');
		await flushPromises();

		expect(wrapper.find('#coordination').exists()).toBe(true);
	});
});

describe('OverviewView hash scrolling', () => {
	it('scrolls to the anchor of the route hash once the data is there', async () => {
		const { store, router } = await mountOverview();
		await router.push('/#coordination');
		scrollIntoView.mockReset();

		store.status = buildStatus('normal');
		await vi.waitFor(() => expect(scrollIntoView).toHaveBeenCalled());
	});

	it('scrolls again when the hash changes', async () => {
		const { store, router } = await mountOverview();
		store.status = buildStatus('normal');
		await flushPromises();
		scrollIntoView.mockReset();

		await router.push('/#datacenters');

		await vi.waitFor(() => expect(scrollIntoView).toHaveBeenCalledTimes(1));
	});
});
