import type { AxiosInstance } from 'axios';

/**
 * Server time minus browser time. The age of the data compares the server's
 * `observe_time` with a clock, and a wrong browser clock would make stale data
 * look fresh (or fresh data stale). The `Date` header of each response
 * measures the difference; it has a one second resolution.
 */
let offsetMs = 0;

export const serverOffsetMs = (): number => offsetMs;

/** The server's current time, by the last `Date` header; the browser's time before any response. */
export const serverNow = (): number => Date.now() + offsetMs;

export const recordServerDate = (header: unknown): void => {
	if (typeof header !== 'string') return;
	const serverTime = Date.parse(header);
	if (Number.isNaN(serverTime)) return;
	offsetMs = serverTime - Date.now();
};

export const resetServerClock = (): void => {
	offsetMs = 0;
};

export const installServerClock = (instance: AxiosInstance): void => {
	instance.interceptors.response.use(
		(response) => {
			recordServerDate(response.headers?.date);
			return response;
		},
		(error) => {
			recordServerDate(error?.response?.headers?.date);
			return Promise.reject(error);
		},
	);
};
