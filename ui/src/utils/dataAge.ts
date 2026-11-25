/**
 * `status.interval` default (seconds). `GetStatus` does not return the
 * interval, so the UI assumes the default; a collector configured with a
 * longer interval makes the data look stale too early.
 */
export const STATUS_INTERVAL_SECONDS = 5;

/** Data older than `3 x status.interval` is shown as stale. */
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

/** Unknown age counts as stale: nothing proves the data is fresh. */
export const isStaleAge = (age: number | null): boolean =>
	age === null || age > STALE_AFTER_SECONDS;

export interface AgeParts {
	unit: 's' | 'm' | 'h';
	value: number;
}

export const formatAge = (seconds: number): AgeParts => {
	if (seconds < 60) return { unit: 's', value: seconds };
	if (seconds < 3600) return { unit: 'm', value: Math.floor(seconds / 60) };
	return { unit: 'h', value: Math.floor(seconds / 3600) };
};

/** Whole seconds of a protobuf duration such as `4.100s`; `null` when missing or malformed. */
export const durationSeconds = (value: string | undefined): number | null => {
	const match = /^(\d+(?:\.\d+)?)s$/.exec(value ?? '');
	return match ? Math.floor(Number(match[1])) : null;
};
