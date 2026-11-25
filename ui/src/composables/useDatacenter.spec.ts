import { AxiosError } from 'axios';
import { createPinia, setActivePinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { type EffectScope, effectScope, nextTick, ref } from 'vue';

import { recordServerDate, resetServerClock } from '@/api/client/serverClock';
import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';
import { STALE_AFTER_SECONDS } from '@/utils/dataAge';
import { POLL_INTERVAL_MS } from '@/utils/poller';
import { useDatacenter } from './useDatacenter';

const getDatacenter = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/datacenter-service/datacenter-service', () => ({
	getDatacenterService: () => ({ getDatacenter }),
}));

const NOW = Date.parse('2026-11-23T10:15:00Z');

const snapshot = (overrides: Partial<Datacenter> = {}): Datacenter => ({
	name: 'datacenters/dc_b',
	state: 'STANDBY',
	observe_time: '2026-11-23T10:14:55Z',
	...overrides,
});

const httpError = (status: number, id?: string) =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		status,
		data: id ? { id } : {},
		statusText: '',
		headers: {},
		config: {} as never,
	});

let scope: EffectScope;
const dc = ref('dc_b');
let result: ReturnType<typeof useDatacenter>;

const staleAfter = ref<number | undefined>(undefined);

const setup = () => {
	scope = effectScope();
	result = scope.run(() =>
		useDatacenter(dc, () => staleAfter.value ?? STALE_AFTER_SECONDS),
	) as ReturnType<typeof useDatacenter>;
};

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(NOW);
	setActivePinia(createPinia());
	vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
	getDatacenter.mockReset();
	getDatacenter.mockResolvedValue(snapshot());
	dc.value = 'dc_b';
	staleAfter.value = undefined;
});

afterEach(() => {
	scope.stop();
	resetServerClock();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('useDatacenter', () => {
	it('has no data and no error until the first answer', () => {
		setup();
		expect(result.datacenter.value).toBeUndefined();
		expect(result.error.value).toBeNull();
		expect(result.notFound.value).toBe(false);
		expect(result.age.value).toBeNull();
	});

	it('requests the datacenter of the route at once and then every 5 seconds', async () => {
		setup();
		await vi.advanceTimersByTimeAsync(0);
		expect(getDatacenter).toHaveBeenCalledTimes(1);
		expect(getDatacenter.mock.calls[0][0]).toBe('dc_b');
		expect(result.datacenter.value?.state).toBe('STANDBY');

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getDatacenter).toHaveBeenCalledTimes(2);
	});

	it('escapes the name so it stays one path segment', async () => {
		setup();
		dc.value = '../status';
		await nextTick();
		await vi.advanceTimersByTimeAsync(0);

		expect(getDatacenter.mock.calls.at(-1)?.[0]).toBe('..%2Fstatus');
	});

	it('passes the timeout and the abort signal to the request', async () => {
		setup();
		await vi.advanceTimersByTimeAsync(0);

		const options = getDatacenter.mock.calls[0][1];
		expect(options.timeout).toBe(2 * POLL_INTERVAL_MS);
		expect(options.signal).toBeInstanceOf(AbortSignal);
	});

	it('keeps the last data when a poll fails and lets its age grow', async () => {
		setup();
		await vi.advanceTimersByTimeAsync(0);
		getDatacenter.mockRejectedValue(httpError(503));

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 4);

		expect(result.datacenter.value?.state).toBe('STANDBY');
		expect(result.error.value).toBeTruthy();
		expect(result.age.value).toBe(25);
		expect(result.isStale.value).toBe(true);
	});

	it('recovers after a failed poll', async () => {
		getDatacenter.mockRejectedValueOnce(httpError(503));
		setup();
		await vi.advanceTimersByTimeAsync(0);
		expect(result.error.value).toBeTruthy();

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);

		expect(result.error.value).toBeNull();
		expect(result.datacenter.value).toBeDefined();
	});

	it('reports a missing datacenter by the error id and stops polling', async () => {
		getDatacenter.mockRejectedValue(
			httpError(404, 'dcctl.datacenter.not_found'),
		);
		setup();
		await vi.advanceTimersByTimeAsync(0);

		expect(result.notFound.value).toBe(true);
		expect(result.datacenter.value).toBeUndefined();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);
		expect(getDatacenter).toHaveBeenCalledTimes(1);
	});

	it('does not take any other 404 for a missing datacenter', async () => {
		getDatacenter.mockRejectedValue(httpError(404));
		setup();
		await vi.advanceTimersByTimeAsync(0);

		expect(result.notFound.value).toBe(false);
		expect(result.error.value).toBeTruthy();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getDatacenter).toHaveBeenCalledTimes(2);
	});

	it('drops the data and starts again when the route changes', async () => {
		setup();
		await vi.advanceTimersByTimeAsync(0);
		let resolve!: (value: Datacenter) => void;
		getDatacenter.mockReturnValueOnce(
			new Promise<Datacenter>((r) => {
				resolve = r;
			}),
		);

		dc.value = 'dc_c';
		await nextTick();
		expect(result.datacenter.value).toBeUndefined();
		expect(getDatacenter.mock.calls.at(-1)?.[0]).toBe('dc_c');

		resolve(snapshot({ name: 'datacenters/dc_c' }));
		await vi.advanceTimersByTimeAsync(0);
		expect(result.datacenter.value?.name).toBe('datacenters/dc_c');
	});

	it('ignores a late answer for the previous datacenter', async () => {
		let resolve!: (value: Datacenter) => void;
		getDatacenter.mockReturnValueOnce(
			new Promise<Datacenter>((r) => {
				resolve = r;
			}),
		);
		getDatacenter.mockResolvedValue(snapshot({ name: 'datacenters/dc_c' }));
		setup();
		dc.value = 'dc_c';
		await nextTick();
		await vi.advanceTimersByTimeAsync(0);

		resolve(snapshot({ name: 'datacenters/dc_b' }));
		await vi.advanceTimersByTimeAsync(0);

		expect(result.datacenter.value?.name).toBe('datacenters/dc_c');
	});

	it('stops polling with its scope', async () => {
		setup();
		await vi.advanceTimersByTimeAsync(0);
		scope.stop();

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(getDatacenter).toHaveBeenCalledTimes(1);
	});

	it('takes the stale threshold from the caller', async () => {
		getDatacenter.mockResolvedValue(
			snapshot({ observe_time: '2026-11-23T10:14:40Z' }),
		);
		staleAfter.value = 30;
		setup();
		await vi.advanceTimersByTimeAsync(0);
		expect(result.age.value).toBe(20);
		expect(result.isStale.value).toBe(false);

		staleAfter.value = 10;
		expect(result.isStale.value).toBe(true);
	});

	it('measures the age with the server clock when the browser is off', async () => {
		const skew = 5 * 60 * 1000;
		recordServerDate(new Date(NOW - skew).toUTCString());
		getDatacenter.mockResolvedValue(
			snapshot({ observe_time: new Date(NOW - skew - 3000).toISOString() }),
		);
		setup();
		await vi.advanceTimersByTimeAsync(0);

		expect(result.age.value).toBe(3);
		expect(result.isStale.value).toBe(false);
	});
});
