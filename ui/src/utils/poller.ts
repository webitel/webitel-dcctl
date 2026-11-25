import { type Ref, ref } from 'vue';

export const POLL_INTERVAL_MS = 5000;
const CLOCK_TICK_MS = 1000;
/** A request that hangs must not stop the polling: give up after two intervals. */
const REQUEST_TIMEOUT_MS = 2 * POLL_INTERVAL_MS;

const isTabVisible = () => document.visibilityState === 'visible';

export interface PollRequestOptions {
	timeout: number;
	signal: AbortSignal;
}

export interface Poller {
	/** Wall clock for data ages; moves once a second while polling. */
	now: Ref<number>;
	start: () => void;
	/** Stops the loop and aborts the request in flight; its answer is dropped. */
	stop: () => void;
	/** Requests now; a call during a request joins it, so requests never overlap. */
	refresh: () => Promise<void>;
}

/**
 * Polls `request` every 5 s while the tab is visible. The handlers get the
 * answers of the current session only: after `stop()` a late answer is dropped.
 */
export const createPoller = <T>({
	request,
	onData,
	onError,
}: {
	request: (options: PollRequestOptions) => Promise<T>;
	onData: (value: T) => void;
	onError: (cause: unknown) => void;
}): Poller => {
	const now = ref(Date.now());
	let started = false;
	// Bumped by stop(): a request that finishes after it belongs to a dead session.
	let epoch = 0;
	let inFlight: Promise<void> | null = null;
	let abort: AbortController | null = null;
	let pollTimer: ReturnType<typeof setTimeout> | undefined;
	let clockTimer: ReturnType<typeof setInterval> | undefined;

	const clearPollTimer = () => {
		clearTimeout(pollTimer);
		pollTimer = undefined;
	};

	const schedule = () => {
		clearPollTimer();
		if (started && isTabVisible()) {
			pollTimer = setTimeout(refresh, POLL_INTERVAL_MS);
		}
	};

	const refresh = (): Promise<void> => {
		if (inFlight) return inFlight;
		clearPollTimer();
		const requestEpoch = epoch;
		const controller = new AbortController();
		abort = controller;
		const current = (async () => {
			try {
				const result = await request({
					timeout: REQUEST_TIMEOUT_MS,
					signal: controller.signal,
				});
				if (requestEpoch !== epoch) return;
				onData(result);
			} catch (cause) {
				if (requestEpoch !== epoch) return;
				onError(cause);
			} finally {
				if (requestEpoch === epoch) {
					inFlight = null;
					now.value = Date.now();
					schedule();
				}
			}
		})();
		inFlight = current;
		return current;
	};

	const onVisibilityChange = () => {
		if (!started) return;
		if (isTabVisible()) {
			refresh();
		} else {
			clearPollTimer();
		}
	};

	const start = () => {
		if (started) return;
		started = true;
		document.addEventListener('visibilitychange', onVisibilityChange);
		clockTimer = setInterval(() => {
			now.value = Date.now();
		}, CLOCK_TICK_MS);
		if (isTabVisible()) refresh();
	};

	const stop = () => {
		started = false;
		epoch++;
		inFlight = null;
		abort?.abort();
		abort = null;
		clearPollTimer();
		clearInterval(clockTimer);
		clockTimer = undefined;
		document.removeEventListener('visibilitychange', onVisibilityChange);
	};

	return { now, start, stop, refresh };
};
