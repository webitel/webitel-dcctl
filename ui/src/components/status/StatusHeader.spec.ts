import { flushPromises } from '@vue/test-utils';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { useStatusStore } from '@/stores/status';
import { mountWithApp } from '@/tests/mountWithApp';
import StatusHeader from './StatusHeader.vue';

const NOW = Date.parse('2026-11-23T10:15:00Z');

const statusObservedAgo = (seconds: number): Status => ({
	observer: { datacenter: 'datacenters/dc_b', node: 'dcctl-b1' },
	observe_time: new Date(NOW - seconds * 1000).toISOString(),
});

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(NOW);
});

afterEach(() => {
	vi.useRealTimers();
});

const mountHeader = async (status?: Status, error: unknown = null) => {
	const { wrapper, pinia } = await mountWithApp(StatusHeader);
	const store = useStatusStore(pinia);
	store.now = NOW;
	store.status = status;
	store.error = error;
	await flushPromises();
	return { wrapper, store };
};

describe('StatusHeader', () => {
	it('shows the view of the observer instance', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(2));

		expect(wrapper.text()).toContain('View from dc_b');
	});

	it('shows how old the data is', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(2));

		expect(wrapper.text()).toContain('Updated 2 s ago');
	});

	it('keeps the age neutral while the data is fresh', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(15));

		expect(wrapper.find('[data-test="data-age"]').classes()).not.toContain(
			'data-age--stale',
		);
	});

	it('greys the age and warns when the data is older than 3 x the interval', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(16));

		const age = wrapper.find('[data-test="data-age"]');
		expect(age.classes()).toContain('data-age--stale');
		expect(age.attributes('title')).toContain('older than 15 s');
		expect(age.text()).toContain('Data may be stale');
	});

	it('does not say the data may be stale while it is fresh', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(2));

		expect(wrapper.text()).not.toContain('may be stale');
	});

	it('shows the deployment profile next to the observer', async () => {
		const { wrapper } = await mountHeader({
			...statusObservedAgo(2),
			profile: 'WARM_STANDBY',
		});

		expect(wrapper.find('[data-test="profile"]').text()).toBe('warm standby');
		expect(wrapper.find('[data-test="observer"]').text()).toBe(
			'View from dc_b',
		);
	});

	it('greys the age as the clock moves on while no new data arrives', async () => {
		const { wrapper, store } = await mountHeader(statusObservedAgo(10));
		expect(wrapper.find('[data-test="data-age"]').classes()).not.toContain(
			'data-age--stale',
		);

		store.now = NOW + 10_000;
		await flushPromises();

		expect(wrapper.find('[data-test="data-age"]').classes()).toContain(
			'data-age--stale',
		);
		expect(wrapper.text()).toContain('Updated 20 s ago');
	});

	it('shows an error state next to the last data when a poll fails', async () => {
		const { wrapper } = await mountHeader(
			statusObservedAgo(7),
			new Error('Network Error'),
		);

		expect(wrapper.find('[data-test="poll-error"]').exists()).toBe(true);
		expect(wrapper.text()).toContain('Cannot refresh the status');
		expect(wrapper.text()).toContain('View from dc_b');
		expect(wrapper.text()).toContain('Updated 7 s ago');
	});

	it('puts the reason of the failure into the error chip', async () => {
		const { wrapper } = await mountHeader(
			statusObservedAgo(7),
			new Error('Network Error'),
		);

		const chip = wrapper.find('[data-test="poll-error"]');
		expect(chip.attributes('title')).toContain('Network Error');
		expect(chip.attributes('title')).toContain('Showing the last known data');
	});

	it('does not promise the last known data when there is none', async () => {
		const { wrapper } = await mountHeader(
			undefined,
			new Error('Network Error'),
		);

		const chip = wrapper.find('[data-test="poll-error"]');
		expect(chip.attributes('title')).toContain('Network Error');
		expect(chip.attributes('title')).not.toContain('last known data');
	});

	it('shows no error state when the poll succeeds', async () => {
		const { wrapper } = await mountHeader(statusObservedAgo(1));

		expect(wrapper.find('[data-test="poll-error"]').exists()).toBe(false);
	});

	it('says there is no data before the first answer', async () => {
		const { wrapper } = await mountHeader(undefined);

		expect(wrapper.text()).toContain('No data yet');
	});
});
