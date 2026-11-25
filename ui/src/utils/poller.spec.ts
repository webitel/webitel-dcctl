import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createPoller, POLL_INTERVAL_MS } from './poller';

const NOW = Date.parse('2026-11-23T10:15:00Z');

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

const request = vi.fn();
const onData = vi.fn();
const onError = vi.fn();

let poller: ReturnType<typeof createPoller<string>>;

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(NOW);
	request.mockReset();
	request.mockResolvedValue('data');
	onData.mockReset();
	onError.mockReset();
	vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('visible');
	poller = createPoller<string>({ request, onData, onError });
});

afterEach(() => {
	poller.stop();
	vi.useRealTimers();
	vi.restoreAllMocks();
});

describe('createPoller', () => {
	it('requests at once and then every 5 seconds', async () => {
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(request).toHaveBeenCalledTimes(2);
		expect(onData).toHaveBeenCalledWith('data');
		expect(POLL_INTERVAL_MS).toBe(5000);
	});

	it('does not start a second loop when started twice', async () => {
		poller.start();
		poller.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);

		expect(request).toHaveBeenCalledTimes(2);
	});

	it('never overlaps requests: a slow answer delays the next one', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 4);
		expect(request).toHaveBeenCalledTimes(1);

		slow.resolve('late');
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(request).toHaveBeenCalledTimes(2);
	});

	it('refresh() during a request returns the same request', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		const first = poller.refresh();
		const second = poller.refresh();
		slow.resolve('x');
		await Promise.all([first, second]);

		expect(request).toHaveBeenCalledTimes(1);
	});

	it('reports a failure and keeps polling', async () => {
		request.mockRejectedValueOnce(new Error('boom'));
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		expect(onError).toHaveBeenCalledTimes(1);

		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(request).toHaveBeenCalledTimes(2);
		expect(onData).toHaveBeenCalledTimes(1);
	});

	it('lets the handler stop the polling', async () => {
		onError.mockImplementation(() => poller.stop());
		request.mockRejectedValue(new Error('gone'));
		poller.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(request).toHaveBeenCalledTimes(1);
	});
});

describe('tab visibility', () => {
	it('pauses while hidden and refreshes at once when shown', async () => {
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 5);
		expect(request).toHaveBeenCalledTimes(1);

		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(2);
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(request).toHaveBeenCalledTimes(3);
	});

	it('does not poll when started in a hidden tab, until it is shown', async () => {
		vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
		poller.start();
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 2);
		expect(request).not.toHaveBeenCalled();

		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(1);
	});

	it('does not reschedule an answer that arrives after the tab was hidden', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');

		slow.resolve('x');
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(request).toHaveBeenCalledTimes(1);
		expect(onData).toHaveBeenCalledWith('x');
	});

	it('does not run a second request when shown again during a request', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		setVisibility('hidden');
		setVisibility('visible');
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(1);

		slow.resolve('x');
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS);
		expect(request).toHaveBeenCalledTimes(2);
	});
});

describe('timeout and abort', () => {
	it('asks for a timeout of two intervals and gives an abort signal', async () => {
		poller.start();
		await vi.advanceTimersByTimeAsync(0);

		const options = request.mock.calls[0][0];
		expect(options.timeout).toBe(2 * POLL_INTERVAL_MS);
		expect(options.signal).toBeInstanceOf(AbortSignal);
	});

	it('aborts the request in flight on stop()', async () => {
		request.mockReturnValueOnce(new Promise(() => {}));
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		const { signal } = request.mock.calls[0][0];
		expect(signal.aborted).toBe(false);

		poller.stop();

		expect(signal.aborted).toBe(true);
	});
});

describe('epoch', () => {
	it('drops an answer that arrives after stop()', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(0);

		poller.stop();
		slow.resolve('late');
		await vi.advanceTimersByTimeAsync(POLL_INTERVAL_MS * 3);

		expect(onData).not.toHaveBeenCalled();
		expect(request).toHaveBeenCalledTimes(1);
	});

	it('drops a late failure after stop()', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(0);

		poller.stop();
		slow.reject(new Error('late'));
		await vi.advanceTimersByTimeAsync(0);

		expect(onError).not.toHaveBeenCalled();
	});

	it('does not let a request of the old session block the new one', async () => {
		const slow = deferred<string>();
		request.mockReturnValueOnce(slow.promise);
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		poller.stop();

		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		expect(request).toHaveBeenCalledTimes(2);

		slow.resolve('old');
		await vi.advanceTimersByTimeAsync(0);
		expect(onData).toHaveBeenCalledTimes(1);
		expect(onData).toHaveBeenCalledWith('data');
	});
});

describe('clock', () => {
	it('moves once a second while running and stops with stop()', async () => {
		poller.start();
		await vi.advanceTimersByTimeAsync(0);
		const first = poller.now.value;

		await vi.advanceTimersByTimeAsync(1000);
		expect(poller.now.value).toBe(first + 1000);

		poller.stop();
		await vi.advanceTimersByTimeAsync(3000);
		expect(poller.now.value).toBe(first + 1000);
	});
});
