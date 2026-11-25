/**
 * Fallback for a status without `interval` (an older server): the default
 * `status.interval` in seconds.
 */
export const STATUS_INTERVAL_SECONDS = 5;

/** Stale threshold used while the status has not told the interval. */
export const STALE_AFTER_SECONDS = 3 * STATUS_INTERVAL_SECONDS;

/**
 * Seconds since the collection cycle finished, `null` when unknown. The result
 * is never negative: the browser clock may run behind the server's.
 */
export const ageSeconds = (
	observeTime: string | undefined,
	nowMs: number,
): number | null => {
	const observed = Date.parse(observeTime ?? '');
	if (Number.isNaN(observed)) return null;
	return Math.max(0, Math.floor((nowMs - observed) / 1000));
};

/** Unknown age counts as stale: nothing proves the data is fresh. `staleAfter` comes from `staleAfterSeconds`. */
export const isStaleAge = (
	age: number | null,
	staleAfter: number = STALE_AFTER_SECONDS,
): boolean => age === null || age > staleAfter;

export interface AgeParts {
	unit: 's' | 'm' | 'h';
	value: number;
}

export const formatAge = (seconds: number): AgeParts => {
	if (seconds < 60) return { unit: 's', value: seconds };
	if (seconds < 3600) return { unit: 'm', value: Math.floor(seconds / 60) };
	return { unit: 'h', value: Math.floor(seconds / 3600) };
};

/** Seconds of a protobuf duration such as `4.100s`; `null` when missing or malformed. */
const parseDuration = (value: string | undefined): number | null => {
	const match = /^(\d+(?:\.\d+)?)s$/.exec(value ?? '');
	return match ? Number(match[1]) : null;
};

/** Whole seconds of a protobuf duration such as `4.100s`; `null` when missing or malformed. */
export const durationSeconds = (value: string | undefined): number | null => {
	const seconds = parseDuration(value);
	return seconds === null ? null : Math.floor(seconds);
};

/**
 * Data older than `3 x (interval + cycle_duration)` is stale: the next cycle
 * ends about that long after `observe_time`. Without `interval` the default
 * applies; a missing `cycle_duration` counts as 0.
 */
export const staleAfterSeconds = (
	status: { interval?: string; cycle_duration?: string } | undefined,
): number => {
	const interval = parseDuration(status?.interval);
	if (interval === null) return STALE_AFTER_SECONDS;
	return 3 * (interval + (parseDuration(status?.cycle_duration) ?? 0));
};
