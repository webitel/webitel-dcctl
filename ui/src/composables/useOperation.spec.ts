import { AxiosError } from 'axios';
import { createPinia, setActivePinia } from 'pinia';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { effectScope, ref } from 'vue';

import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { useAuthStore } from '@/stores/auth';
import { IDLE_REREAD_MS, reconnectDelayMs, useOperation } from './useOperation';

const getOperation = vi.hoisted(() => vi.fn());
vi.mock('@/api/gen/operation-service/operation-service', () => ({
	getOperationService: () => ({ getOperation }),
}));

vi.mock('@/api/gen/status-service/status-service', () => ({
	getStatusService: () => ({ getStatus: async () => ({}) }),
}));

const watch = vi.hoisted(() => vi.fn());
vi.mock('@/api/client/watchStream', async (importActual) => ({
	...(await importActual<typeof import('@/api/client/watchStream')>()),
	watchOperation: watch,
}));

const { WatchHttpError } = await import('@/api/client/watchStream');

const NOW = Date.parse('2026-11-23T10:15:00Z');

const op = (
	state: Operation['state'],
	second: number,
	extra: Operation = {},
): Operation => ({
	name: 'operations/a',
	state,
	update_time: new Date(NOW + second * 1000).toISOString(),
	...extra,
});

const notFound = () =>
	new AxiosError('failed', 'ERR_BAD_REQUEST', undefined, null, {
		status: 404,
		data: { id: 'dcctl.operation.not_found', code: 404 },
		statusText: '',
		headers: {},
		config: {} as never,
	});

interface Stream {
	push: (operation: Operation) => void;
	end: () => void;
	fail: (cause: unknown) => void;
	signal: AbortSignal;
}

/** Each call of the watch is one controllable stream. */
const streams: Stream[] = [];
const nextStream = () => streams[streams.length - 1] as Stream;

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(NOW);
	setActivePinia(createPinia());
	sessionStorage.setItem(
		'dcctl.auth',
		JSON.stringify({ name: 's', token: 'dG9rZW4=' }),
	);
	useAuthStore();
	getOperation.mockReset();
	watch.mockReset();
	streams.length = 0;
	watch.mockImplementation(
		(
			_id: string,
			options: { signal: AbortSignal; onOperation: (op: Operation) => void },
		) =>
			new Promise<void>((resolve, reject) => {
				const stream: Stream = {
					push: options.onOperation,
					end: resolve,
					fail: reject,
					signal: options.signal,
				};
				options.signal.addEventListener('abort', () =>
					reject(new DOMException('aborted', 'AbortError')),
				);
				streams.push(stream);
			}),
	);
});

afterEach(() => {
	vi.useRealTimers();
	sessionStorage.clear();
});

const mount = (id = 'a') => {
	const scope = effectScope();
	const state = scope.run(() => useOperation(id)) as ReturnType<
		typeof useOperation
	>;
	return { scope, state };
};

const settle = () => vi.advanceTimersByTimeAsync(0);

describe('useOperation', () => {
	it('is loading until the operation is read', async () => {
		getOperation.mockReturnValue(new Promise(() => undefined));
		const { state } = mount();
		await settle();
		expect(state.phase.value).toBe('loading');
		expect(state.operation.value).toBeUndefined();
	});

	it('shows the operation and watches it; the stream makes it live', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		const { state } = mount();
		await settle();
		expect(state.phase.value).toBe('ready');
		expect(state.connection.value).toBe('connecting');
		expect(watch).toHaveBeenCalledWith(
			'a',
			expect.objectContaining({ authorization: 'Basic dG9rZW4=' }),
		);
		nextStream().push(op('RUNNING', 2, { warnings: ['w'] }));
		expect(state.connection.value).toBe('live');
		expect(state.operation.value?.warnings).toEqual(['w']);
	});

	it('escapes the id for the read', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		mount('a/b');
		await settle();
		expect(getOperation).toHaveBeenCalledWith('a%2Fb', expect.anything());
	});

	it('applies the messages of the stream in order', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		const { state } = mount();
		await settle();
		nextStream().push(
			op('RUNNING', 2, { steps: [{ index: 0, state: 'RUNNING' }] }),
		);
		nextStream().push(
			op('RUNNING', 3, { steps: [{ index: 0, state: 'SUCCEEDED' }] }),
		);
		expect(state.operation.value?.steps?.[0]?.state).toBe('SUCCEEDED');
	});

	it('shows an error while the first read fails and retries until it works', async () => {
		getOperation.mockRejectedValueOnce(new Error('Network Error'));
		getOperation.mockResolvedValue(op('RUNNING', 1));
		const { state } = mount();
		await settle();
		expect(state.phase.value).toBe('error');
		expect(state.error.value).toBeInstanceOf(Error);
		await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
		expect(state.phase.value).toBe('ready');
		expect(state.error.value).toBeNull();
	});

	it('reports a missing operation and does not retry', async () => {
		getOperation.mockRejectedValue(notFound());
		const { state } = mount();
		await settle();
		expect(state.phase.value).toBe('notFound');
		await vi.advanceTimersByTimeAsync(60_000);
		expect(getOperation).toHaveBeenCalledTimes(1);
		expect(watch).not.toHaveBeenCalled();
	});

	describe('after a drop', () => {
		it('says it is reconnecting, keeps the data with its age, and reads the operation again before watching', async () => {
			getOperation.mockResolvedValueOnce(op('RUNNING', 1));
			const { state } = mount();
			await settle();
			nextStream().push(
				op('RUNNING', 2, { steps: [{ index: 0, state: 'RUNNING' }] }),
			);
			vi.advanceTimersByTime(7000);
			nextStream().fail(new TypeError('network error'));
			await settle();
			expect(state.connection.value).toBe('reconnecting');
			expect(state.error.value).toBeInstanceOf(TypeError);
			expect(state.operation.value?.steps?.[0]?.state).toBe('RUNNING');
			expect(state.age.value).toBe(7);

			getOperation.mockResolvedValueOnce(
				op('RUNNING', 20, { steps: [{ index: 0, state: 'SUCCEEDED' }] }),
			);
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
			expect(getOperation).toHaveBeenCalledTimes(2);
			expect(state.operation.value?.steps?.[0]?.state).toBe('SUCCEEDED');
			expect(watch).toHaveBeenCalledTimes(2);
			expect(state.connection.value).toBe('reconnecting');
			nextStream().push(op('RUNNING', 21));
			expect(state.connection.value).toBe('live');
			expect(state.error.value).toBeNull();
		});

		it('does not lose a terminal state reached while the stream was down', async () => {
			getOperation.mockResolvedValueOnce(op('RUNNING', 1));
			const { state } = mount();
			await settle();
			nextStream().fail(new TypeError('network error'));
			await settle();
			getOperation.mockResolvedValueOnce(op('SUCCEEDED', 30));
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
			expect(state.operation.value?.state).toBe('SUCCEEDED');
			expect(state.connection.value).toBe('ended');
			await vi.advanceTimersByTimeAsync(60_000);
			expect(watch).toHaveBeenCalledTimes(1);
			expect(getOperation).toHaveBeenCalledTimes(2);
		});

		it('waits longer after each failure, up to a cap, and starts over after a message', async () => {
			expect([0, 1, 2, 3, 4, 5, 6].map(reconnectDelayMs)).toEqual([
				1000, 2000, 4000, 8000, 10000, 10000, 10000,
			]);
			getOperation.mockResolvedValue(op('RUNNING', 1));
			mount();
			await settle();
			nextStream().fail(new Error('x'));
			await settle();
			await vi.advanceTimersByTimeAsync(999);
			expect(watch).toHaveBeenCalledTimes(1);
			await vi.advanceTimersByTimeAsync(1);
			expect(watch).toHaveBeenCalledTimes(2);
			nextStream().fail(new Error('x'));
			await settle();
			await vi.advanceTimersByTimeAsync(1999);
			expect(watch).toHaveBeenCalledTimes(2);
			await vi.advanceTimersByTimeAsync(1);
			expect(watch).toHaveBeenCalledTimes(3);
			nextStream().push(op('RUNNING', 5));
			nextStream().push(op('RUNNING', 6));
			nextStream().fail(new Error('x'));
			await settle();
			await vi.advanceTimersByTimeAsync(1000);
			expect(watch).toHaveBeenCalledTimes(4);
		});

		it('keeps backing off when the server closes right after the snapshot', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			mount();
			await settle();
			for (const delay of [1000, 2000, 4000, 8000, 10_000, 10_000]) {
				const before = watch.mock.calls.length;
				nextStream().push(op('RUNNING', 2));
				nextStream().end();
				await settle();
				await vi.advanceTimersByTimeAsync(delay - 1);
				expect(watch).toHaveBeenCalledTimes(before);
				await vi.advanceTimersByTimeAsync(1);
				expect(watch).toHaveBeenCalledTimes(before + 1);
			}
			// 6 closes in about 35 s, not one a second.
			expect(getOperation.mock.calls.length).toBe(watch.mock.calls.length);
		});

		it('starts over when the stream lived long enough', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			mount();
			await settle();
			nextStream().fail(new Error('x'));
			await settle();
			await vi.advanceTimersByTimeAsync(1000);
			nextStream().push(op('RUNNING', 2));
			await vi.advanceTimersByTimeAsync(10_000);
			nextStream().end();
			await settle();
			await vi.advanceTimersByTimeAsync(1000);
			expect(watch).toHaveBeenCalledTimes(3);
		});

		it('keeps trying when the re-read fails too, without losing the data', async () => {
			getOperation.mockResolvedValueOnce(
				op('RUNNING', 1, { warnings: ['kept'] }),
			);
			const { state } = mount();
			await settle();
			nextStream().fail(new Error('x'));
			await settle();
			getOperation.mockRejectedValueOnce(new Error('still down'));
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
			expect(state.connection.value).toBe('reconnecting');
			expect(state.operation.value?.warnings).toEqual(['kept']);
			expect((state.error.value as Error).message).toBe('still down');
			getOperation.mockResolvedValueOnce(op('RUNNING', 9));
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(1));
			expect(watch).toHaveBeenCalledTimes(2);
		});

		it('reconnects after the server closes the stream without a terminal state', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			const { state } = mount();
			await settle();
			nextStream().push(op('RUNNING', 2));
			nextStream().end();
			await settle();
			expect(state.connection.value).toBe('reconnecting');
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
			expect(watch).toHaveBeenCalledTimes(2);
		});

		it('ignores a version older than the one shown', async () => {
			getOperation.mockResolvedValueOnce(
				op('RUNNING', 10, { warnings: ['new'] }),
			);
			const { state } = mount();
			await settle();
			nextStream().fail(new Error('x'));
			await settle();
			getOperation.mockResolvedValueOnce(
				op('RUNNING', 4, { warnings: ['old'] }),
			);
			await vi.advanceTimersByTimeAsync(reconnectDelayMs(0));
			expect(state.operation.value?.warnings).toEqual(['new']);
		});
	});

	describe('terminal state', () => {
		it.each(['SUCCEEDED', 'FAILED', 'ABANDONED'] as const)(
			'stops at %s from the stream',
			async (final) => {
				getOperation.mockResolvedValue(op('RUNNING', 1));
				const { state } = mount();
				await settle();
				nextStream().push(op(final, 5));
				await settle();
				expect(state.connection.value).toBe('ended');
				expect(state.operation.value?.state).toBe(final);
				expect(nextStream().signal.aborted).toBe(true);
				await vi.advanceTimersByTimeAsync(60_000);
				expect(watch).toHaveBeenCalledTimes(1);
				expect(getOperation).toHaveBeenCalledTimes(1);
			},
		);

		it('does not watch an operation that is already final', async () => {
			getOperation.mockResolvedValue(op('FAILED', 1));
			const { state } = mount();
			await settle();
			expect(state.connection.value).toBe('ended');
			expect(watch).not.toHaveBeenCalled();
		});

		it('keeps watching an interrupted operation', async () => {
			getOperation.mockResolvedValue(op('INTERRUPTED', 1));
			const { state } = mount();
			await settle();
			expect(state.connection.value).toBe('connecting');
			expect(watch).toHaveBeenCalledTimes(1);
		});
	});

	describe('abort', () => {
		it('aborts the stream and stops everything when the scope ends', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			const { scope, state } = mount();
			await settle();
			const stream = nextStream();
			scope.stop();
			await settle();
			expect(stream.signal.aborted).toBe(true);
			await vi.advanceTimersByTimeAsync(60_000);
			expect(watch).toHaveBeenCalledTimes(1);
			expect(getOperation).toHaveBeenCalledTimes(1);
			stream.push(op('RUNNING', 9, { warnings: ['late'] }));
			expect(state.operation.value?.warnings).toBeUndefined();
		});

		it('aborts the read in flight and drops its late answer', async () => {
			let resolveRead: (value: Operation) => void = () => undefined;
			let readSignal: AbortSignal | undefined;
			getOperation.mockImplementation(
				(_id: string, options: { signal: AbortSignal }) => {
					readSignal = options.signal;
					return new Promise<Operation>((resolve) => {
						resolveRead = resolve;
					});
				},
			);
			const { scope, state } = mount();
			await settle();
			scope.stop();
			expect(readSignal?.aborted).toBe(true);
			resolveRead(op('RUNNING', 1));
			await settle();
			expect(state.operation.value).toBeUndefined();
			expect(watch).not.toHaveBeenCalled();
		});

		it('does not retry after the scope ends during a backoff', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			const { scope } = mount();
			await settle();
			nextStream().fail(new Error('x'));
			await settle();
			scope.stop();
			await vi.advanceTimersByTimeAsync(60_000);
			expect(getOperation).toHaveBeenCalledTimes(1);
		});

		it('starts over for another operation and drops the data of the previous one', async () => {
			getOperation.mockResolvedValueOnce(
				op('RUNNING', 1, { warnings: ['first'] }),
			);
			const scope = effectScope();
			const id = ref('a');
			const state = scope.run(() => useOperation(() => id.value)) as ReturnType<
				typeof useOperation
			>;
			await settle();
			const first = nextStream();
			expect(state.operation.value?.warnings).toEqual(['first']);
			id.value = 'b';
			getOperation.mockReturnValue(new Promise(() => undefined));
			await settle();
			expect(first.signal.aborted).toBe(true);
			expect(state.operation.value).toBeUndefined();
		});
	});

	describe('401', () => {
		it('logs out and stops when the stream is refused', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			const { state } = mount();
			await settle();
			nextStream().fail(new WatchHttpError(401));
			await settle();
			expect(useAuthStore().isAuthenticated).toBe(false);
			await vi.advanceTimersByTimeAsync(60_000);
			expect(watch).toHaveBeenCalledTimes(1);
			expect(state.connection.value).toBe('ended');
		});
	});

	it('does not log out a login that changed after the stream was opened', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		mount();
		await settle();
		const stream = nextStream();
		await useAuthStore().login('other', 'password');
		stream.fail(new WatchHttpError(401));
		await settle();
		expect(useAuthStore().isAuthenticated).toBe(true);
	});

	it('reconnect() restarts at once: read again, then watch', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		const { state } = mount();
		await settle();
		const first = nextStream();
		nextStream().push(op('RUNNING', 2));
		getOperation.mockResolvedValue(op('RUNNING', 8, { warnings: ['fresh'] }));
		state.reconnect();
		await settle();
		expect(first.signal.aborted).toBe(true);
		expect(getOperation).toHaveBeenCalledTimes(2);
		expect(watch).toHaveBeenCalledTimes(2);
		expect(state.operation.value?.warnings).toEqual(['fresh']);
		expect(state.connection.value).toBe('reconnecting');
	});

	describe('a quiet stream', () => {
		it('is read again after 45 s without a message, and a newer version makes it reconnect', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			const { state } = mount();
			await settle();
			nextStream().push(op('RUNNING', 2));
			await vi.advanceTimersByTimeAsync(IDLE_REREAD_MS - 2000);
			expect(getOperation).toHaveBeenCalledTimes(1);
			getOperation.mockResolvedValue(
				op('FAILED', 40, { warnings: ['moved on'] }),
			);
			const first = nextStream();
			await vi.advanceTimersByTimeAsync(3000);
			expect(getOperation.mock.calls.length).toBeGreaterThanOrEqual(2);
			expect(state.operation.value?.warnings).toEqual(['moved on']);
			expect(first.signal.aborted).toBe(true);
		});

		it('stays live when the read shows nothing newer, and does not read again at once', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 2));
			const { state } = mount();
			await settle();
			nextStream().push(op('RUNNING', 2));
			await vi.advanceTimersByTimeAsync(IDLE_REREAD_MS + 1000);
			expect(getOperation).toHaveBeenCalledTimes(2);
			expect(state.connection.value).toBe('live');
			expect(watch).toHaveBeenCalledTimes(1);
			expect(state.age.value).toBeLessThan(5);
			await vi.advanceTimersByTimeAsync(10_000);
			expect(getOperation).toHaveBeenCalledTimes(2);
		});

		it('does not read while reconnecting', async () => {
			getOperation.mockResolvedValue(op('RUNNING', 1));
			mount();
			await settle();
			nextStream().push(op('RUNNING', 2));
			nextStream().fail(new Error('x'));
			getOperation.mockRejectedValue(new Error('down'));
			await vi.advanceTimersByTimeAsync(20_000);
			// only the re-reads of the backoff loop, none from the idle check
			expect(getOperation.mock.calls.length).toBeLessThanOrEqual(6);
		});
	});

	it('counts the age of the data from the last message', async () => {
		getOperation.mockResolvedValue(op('RUNNING', 1));
		const { state } = mount();
		await settle();
		expect(state.age.value).toBe(0);
		await vi.advanceTimersByTimeAsync(5000);
		expect(state.age.value).toBe(5);
		nextStream().push(op('RUNNING', 3));
		expect(state.age.value).toBe(0);
	});
});
