import type { MaybeRefOrGetter } from 'vue';
import { computed, onScopeDispose, ref, toValue, watch } from 'vue';

import { getApiError, isUnauthorized } from '@/api/client/apiError';
import { serverOffsetMs } from '@/api/client/serverClock';
import type { WebitelDcctlV1Datacenter as Datacenter } from '@/api/gen/_models';
import { getDatacenterService } from '@/api/gen/datacenter-service/datacenter-service';
import { ageSeconds, isStaleAge, STALE_AFTER_SECONDS } from '@/utils/dataAge';
import { createPoller } from '@/utils/poller';

const NOT_FOUND_ID = 'dcctl.datacenter.not_found';

/**
 * Details of one datacenter, polled while the scope lives. A change of `dc`
 * drops the data of the previous one. `staleAfter` is the age in seconds past
 * which the data is stale. The state is told apart by the error
 * `id`: any other failure keeps the last data and shows its age.
 */
export const useDatacenter = (
	dc: MaybeRefOrGetter<string>,
	staleAfter: MaybeRefOrGetter<number> = STALE_AFTER_SECONDS,
) => {
	const datacenter = ref<Datacenter>();
	/** Error of the last poll; `null` after a successful one. */
	const error = ref<unknown>(null);
	const notFound = ref(false);

	const poller = createPoller<Datacenter>({
		// The name comes from the URL: escaped, it stays one path segment.
		request: (options) =>
			getDatacenterService().getDatacenter(
				encodeURIComponent(toValue(dc)),
				options,
			),
		onData: (result) => {
			datacenter.value = result;
			error.value = null;
		},
		onError: (cause) => {
			if (getApiError(cause)?.id === NOT_FOUND_ID) {
				stop();
				notFound.value = true;
				return;
			}
			error.value = cause;
			if (isUnauthorized(cause)) stop();
		},
	});

	const stop = () => {
		poller.stop();
		datacenter.value = undefined;
		error.value = null;
		notFound.value = false;
	};

	watch(
		() => toValue(dc),
		() => {
			stop();
			poller.start();
		},
		{ immediate: true },
	);
	onScopeDispose(stop);

	/** Seconds since the observer's last collection cycle; `null` without data. */
	const age = computed(() =>
		datacenter.value
			? ageSeconds(
					datacenter.value.observe_time,
					poller.now.value + serverOffsetMs(),
				)
			: null,
	);
	const isStale = computed(() => isStaleAge(age.value, toValue(staleAfter)));

	return {
		datacenter,
		error,
		notFound,
		now: poller.now,
		age,
		isStale,
		refresh: poller.refresh,
	};
};
