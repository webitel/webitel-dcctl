import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

import { isUnauthorized } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { getStatusService } from '@/api/gen/status-service/status-service';
import { ageSeconds, isStaleAge } from '@/utils/dataAge';
import { isOperationShown } from '@/utils/operationLine';
import { sortAlarms, sortDatacenters } from '@/utils/ordering';

export const POLL_INTERVAL_MS = 5000;
const CLOCK_TICK_MS = 1000;
/** A request that hangs must not stop the polling: give up after two intervals. */
const REQUEST_TIMEOUT_MS = 2 * POLL_INTERVAL_MS;

const isTabVisible = () => document.visibilityState === 'visible';

export const useStatusStore = defineStore('status', () => {
	const status = ref<Status>();
	/** Error of the last poll; `null` after a successful one. */
	const error = ref<unknown>(null);
	/** Wall clock for the data age; moves once a second while polling. */
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

	/** Fetches the status; a call during a request joins it, so requests never overlap. */
	const refresh = (): Promise<void> => {
		if (inFlight) return inFlight;
		clearPollTimer();
		const requestEpoch = epoch;
		const controller = new AbortController();
		abort = controller;
		const request = (async () => {
			try {
				const result = await getStatusService().getStatus({
					timeout: REQUEST_TIMEOUT_MS,
					signal: controller.signal,
				});
				if (requestEpoch !== epoch) return;
				status.value = result;
				error.value = null;
			} catch (cause) {
				if (requestEpoch !== epoch) return;
				// Keep the last data on screen: its age shows that it is old.
				error.value = cause;
				if (isUnauthorized(cause)) stop();
			} finally {
				if (requestEpoch === epoch) {
					inFlight = null;
					now.value = Date.now();
					schedule();
				}
			}
		})();
		inFlight = request;
		return request;
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

	/** Stops polling and drops the data: it belongs to the session that ends. */
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
		status.value = undefined;
		error.value = null;
	};

	const alarms = computed(() => sortAlarms(status.value?.alarms ?? []));
	const datacenters = computed(() =>
		sortDatacenters(
			status.value?.datacenters ?? [],
			status.value?.active_datacenter,
		),
	);
	const activeOperation = computed(() =>
		isOperationShown(status.value?.operation)
			? status.value?.operation
			: undefined,
	);
	/** Seconds since the last finished collection cycle; `null` before the first answer. */
	const age = computed(() =>
		status.value
			? ageSeconds(status.value.observe_time, now.value + serverOffsetMs())
			: null,
	);
	const isStale = computed(() => isStaleAge(age.value));

	return {
		status,
		error,
		now,
		alarms,
		datacenters,
		activeOperation,
		age,
		isStale,
		start,
		stop,
		refresh,
	};
});
