import { defineStore } from 'pinia';
import { computed, ref } from 'vue';

import { isUnauthorized } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import type { WebitelDcctlV1Status as Status } from '@/api/gen/_models';
import { getStatusService } from '@/api/gen/status-service/status-service';
import { ageSeconds, isStaleAge, staleAfterSeconds } from '@/utils/dataAge';
import { isOperationShown } from '@/utils/operationLine';
import { sortAlarms, sortDatacenters } from '@/utils/ordering';
import { createPoller, POLL_INTERVAL_MS } from '@/utils/poller';

export { POLL_INTERVAL_MS };

export const useStatusStore = defineStore('status', () => {
	const status = ref<Status>();
	/** Error of the last poll; `null` after a successful one. */
	const error = ref<unknown>(null);

	const poller = createPoller<Status>({
		request: (options) => getStatusService().getStatus(options),
		onData: (result) => {
			status.value = result;
			error.value = null;
		},
		onError: (cause) => {
			// Keep the last data on screen: its age shows that it is old.
			error.value = cause;
			if (isUnauthorized(cause)) stop();
		},
	});
	const { now, start, refresh } = poller;

	/** Stops polling and drops the data: it belongs to the session that ends. */
	const stop = () => {
		poller.stop();
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
	/** Older than this many seconds is stale: from the status, the default before it. */
	const staleAfter = computed(() => staleAfterSeconds(status.value));
	const isStale = computed(() => isStaleAge(age.value, staleAfter.value));

	return {
		status,
		error,
		now,
		alarms,
		datacenters,
		activeOperation,
		age,
		staleAfter,
		isStale,
		start,
		stop,
		refresh,
	};
});
