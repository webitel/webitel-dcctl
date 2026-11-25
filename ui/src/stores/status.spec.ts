import { AxiosError } from 'axios';
import { createPinia, setActivePinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { recordServerDate, resetServerClock } from '@/api/client/serverClock';
import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { POLL_INTERVAL_MS, useStatusStore } from './status';

const getStatus = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/status-service/status-service', () => ({
	getStatusService: () => ({ getStatus }),
}));

const NOW = Date.parse('2026-11-23T10:15:00Z');

const snapshot = (overrides: Partial<Status> = {}): Status => ({
	observe_time: '2026-11-23T10:14:55Z',
	active_datacenter: 'datacenters/dc_a',
	datacenters: [
		{ name: 'datacenters/dc_c' },
		{ name: 'datacenters/dc_b' },
		{ name: 'datacenters/dc_a' },
	],
	alarms: [
		{ severity: 'WARNING', start_time: '2026-11-23T10:00:00Z' },
		{ severity: 'CRITICAL', start_time: '2026-11-23T10:10:00Z' },
	],
	...overrides,
});

const httpError = (status: number) =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		status,
		data: {},
		statusText: '',
		headers: {},
		config: {} as never,
	});

const deferred = <T>() => {
	let resolve!: (value: T) => void;
	let reject!: (reason: unknown) => void;
	const promise = new Promise<T>((res, rej) => {
		resolve = res;
		reject = rej;
	});
	return { promise, resolve, reject };
};

const setVisibility = (state: 'visible' | 'hidden') => {
	vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(state);
	document.dispatchEvent(new Event('visibilitychange'));
};

let store: ReturnType<typeof useStatusStore>;

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(NOW);
	setActivePinia(createPinia());
	getStatus.mockReset();
	getStatus.mockResolvedValue(snapshot());
	vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
	store = useStatusStore();
});

afterEach(() => {
	store.stop();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('polling', () => {
	it('requests the status at once and then every 5 seconds', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		expect(getStatus).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(2);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(3);
		expect(POLL_INTERVAL_MS).toBe(5000);
	});

	it('does not start a second poll loop when started twice', async () => {
		store.start();
		store.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);

		expect(getStatus).toHaveBeenCalledTimes(2);
	});

	it('never overlaps requests: a slow answer delays the next poll', async () => {
		const slow = deferred<Status>();
		getStatus.mockReturnValueOnce(slow.promise);
		store.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 4);
		expect(getStatus).toHaveBeenCalledTimes(1);

		slow.resolve(snapshot());
		await vi.advanceTimersByTimeAsync(0);
		expect(getStatus).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(2);
	});

	it('refresh() during a request returns the same request', async () => {
		const slow = deferred<Status>();
		getStatus.mockReturnValueOnce(slow.promise);
		const first = store.refresh();
		const second = store.refresh();
		slow.resolve(snapshot());
		await Promise.all([first, second]);

		expect(getStatus).toHaveBeenCalledTimes(1);
	});
});

describe('tab visibility', () => {
	it('stops polling while the tab is hidden', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5);

		expect(getStatus).toHaveBeenCalledTimes(1);
	});

	it('refreshes at once when the tab becomes visible again', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5);

		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);

		expect(getStatus).toHaveBeenCalledTimes(2);
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(3);
	});

	it('does not poll when started in a hidden tab, until it is shown', async () => {
		vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
		store.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 2);
		expect(getStatus).not.toHaveBeenCalled();

		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(getStatus).toHaveBeenCalledTimes(1);
	});

	it('does not reschedule an answer that arrives after the tab was hidden', async () => {
		const slow = deferred<Status>();
		getStatus.mockReturnValueOnce(slow.promise);
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');

		slow.resolve(snapshot());
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(getStatus).toHaveBeenCalledTimes(1);
		expect(store.status).toBeDefined();
	});

	it('does not run a second request when shown again during a request', async () => {
		const slow = deferred<Status>();
		getStatus.mockReturnValueOnce(slow.promise);
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');
		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);

		expect(getStatus).toHaveBeenCalledTimes(1);

		slow.resolve(snapshot());
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(2);
	});
});

describe('failed poll', () => {
	it('keeps the last data, records the error and keeps polling', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		const before = store.status;
		getStatus.mockRejectedValueOnce(httpError(503));

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);

		expect(store.status).toEqual(before);
		expect(store.error).toBeTruthy();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(3);
		expect(store.error).toBeNull();
	});

	it('lets the data age grow while polls fail', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		expect(store.age).toBe(5);
		expect(store.isStale).toBe(false);
		getStatus.mockRejectedValue(httpError(503));

		await vi.advanceTimersByTimeAsync(20_000);

		expect(store.age).toBe(25);
		expect(store.isStale).toBe(true);
	});

	it('stops polling and drops the data on 401', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		getStatus.mockRejectedValue(httpError(401));

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(2);
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(getStatus).toHaveBeenCalledTimes(2);
		expect(store.status).toBeUndefined();
	});
});

describe('hung request', () => {
	// Stands in for axios: rejects after `options.timeout` like a real timeout.
	const hangUntilTimeout = (options?: { timeout?: number }) =>
		new Promise<Status>((_resolve, reject) => {
			setTimeout(
				() => reject(new AxiosError('timeout', 'ECONNABORTED')),
				options?.timeout,
			);
		});

	it('asks for a timeout of two poll intervals and gives an abort signal', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);

		const options = getStatus.mock.calls[0][0];
		expect(options.timeout).toBe(2 * POLL_INTERVAL_MS);
		expect(options.signal).toBeInstanceOf(AbortSignal);
	});

	it('records the error after the timeout and polls again', async () => {
		getStatus.mockImplementationOnce((options) => hangUntilTimeout(options));
		store.start();
		await vi.advanceTimersByTimeAsync(2 * POLL_INTERVAL_MS - 1);
		expect(store.error).toBeNull();
		expect(getStatus).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(1);
		expect(store.error).toBeTruthy();

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(getStatus).toHaveBeenCalledTimes(2);
		expect(store.error).toBeNull();
	});

	it('aborts the request in flight on stop()', async () => {
		getStatus.mockReturnValueOnce(new Promise(() => {}));
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		const { signal } = getStatus.mock.calls[0][0];
		expect(signal.aborted).toBe(false);

		store.stop();

		expect(signal.aborted).toBe(true);
	});
});

describe('stop', () => {
	it('stops polling and ignores an answer that arrives later', async () => {
		const slow = deferred<Status>();
		getStatus.mockReturnValueOnce(slow.promise);
		store.start();
		await vi.advanceTimersByTimeAsync(0);

		store.stop();
		slow.resolve(snapshot());
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(getStatus).toHaveBeenCalledTimes(1);
		expect(store.status).toBeUndefined();
	});

	it('can be started again', async () => {
		store.start();
		await vi.advanceTimersByTimeAsync(0);
		store.stop();

		store.start();
		await vi.advanceTimersByTimeAsync(0);

		expect(getStatus).toHaveBeenCalledTimes(2);
		expect(store.status).toBeDefined();
	});
});

describe('data age with a skewed browser clock', () => {
	const FIVE_MIN = 5 * 60 * 1000;

	afterEach(() => resetServerClock());

	// observe_time is 3 s before the server's "now"; the browser is `skew` ms away from the server.
	const pollWithSkew = async (skew: number, observedAgo: number) => {
		const serverNowMs = NOW - skew;
		recordServerDate(new Date(serverNowMs).toUTCString());
		getStatus.mockResolvedValue(
			snapshot({
				observe_time: new Date(serverNowMs - observedAgo * 1000).toISOString(),
			}),
		);
		await store.refresh();
	};

	it.each([
		['behind', -FIVE_MIN],
		['ahead of', FIVE_MIN],
	])(
		'fresh data is fresh when the browser is 5 min %s the server',
		async (_n, skew) => {
			await pollWithSkew(skew, 3);

			expect(store.age).toBe(3);
			expect(store.isStale).toBe(false);
		},
	);

	it.each([
		['behind', -FIVE_MIN],
		['ahead of', FIVE_MIN],
	])(
		'stale data is stale when the browser is 5 min %s the server',
		async (_n, skew) => {
			await pollWithSkew(skew, 60);

			expect(store.age).toBe(60);
			expect(store.isStale).toBe(true);
		},
	);

	it('keeps the browser clock when the server sent no Date header', async () => {
		resetServerClock();
		getStatus.mockResolvedValue(
			snapshot({ observe_time: new Date(NOW - 7000).toISOString() }),
		);
		await store.refresh();

		expect(store.age).toBe(7);
	});
});

describe('stale threshold', () => {
	it('comes from interval and cycle_duration of the status', async () => {
		getStatus.mockResolvedValue(
			snapshot({
				interval: '10s',
				cycle_duration: '2s',
				observe_time: new Date(NOW - 30_000).toISOString(),
			}),
		);
		await store.refresh();

		expect(store.staleAfter).toBe(36);
		expect(store.age).toBe(30);
		expect(store.isStale).toBe(false);
	});

	it('falls back to the default without the fields', async () => {
		getStatus.mockResolvedValue(
			snapshot({ observe_time: new Date(NOW - 30_000).toISOString() }),
		);
		await store.refresh();

		expect(store.staleAfter).toBe(15);
		expect(store.isStale).toBe(true);
	});
});

describe('derived state', () => {
	it('sorts alarms and datacenters', async () => {
		await store.refresh();

		expect(store.alarms.map((alarm) => alarm.severity)).toEqual([
			'CRITICAL',
			'WARNING',
		]);
		expect(store.datacenters.map((dc) => dc.name)).toEqual([
			'datacenters/dc_a',
			'datacenters/dc_b',
			'datacenters/dc_c',
		]);
	});

	it('exposes the operation only while it runs or is interrupted', async () => {
		getStatus.mockResolvedValueOnce(
			snapshot({
				operation: { name: 'operations/1', state: 'RUNNING' },
			}),
		);
		await store.refresh();
		expect(store.activeOperation?.name).toBe('operations/1');

		getStatus.mockResolvedValueOnce(
			snapshot({
				operation: { name: 'operations/1', state: 'SUCCEEDED' },
			}),
		);
		await store.refresh();
		expect(store.activeOperation).toBeUndefined();
	});

	it('has no age before the first answer', () => {
		expect(store.age).toBeNull();
	});
});
