import axios, { type AxiosAdapter, AxiosError } from 'axios';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
	installServerClock,
	recordServerDate,
	resetServerClock,
	serverNow,
	serverOffsetMs,
} from './serverClock';

const BROWSER_NOW = Date.parse('2026-11-23T10:15:00Z');
const FIVE_MIN = 5 * 60 * 1000;

beforeEach(() => {
	vi.useFakeTimers();
	vi.setSystemTime(BROWSER_NOW);
	resetServerClock();
});

afterEach(() => {
	vi.useRealTimers();
});

describe('recordServerDate', () => {
	it('has no offset until a response arrives', () => {
		expect(serverOffsetMs()).toBe(0);
		expect(serverNow()).toBe(BROWSER_NOW);
	});

	it.each([
		['behind', -FIVE_MIN],
		['ahead of', FIVE_MIN],
	])('measures a browser clock %s the server by 5 minutes', (_name, skew) => {
		// the browser clock is `skew` away from the server clock
		const serverTime = new Date(BROWSER_NOW - skew);

		recordServerDate(serverTime.toUTCString());

		expect(serverOffsetMs()).toBe(-skew - (BROWSER_NOW % 1000));
		expect(Math.abs(serverNow() - serverTime.getTime())).toBeLessThan(1000);
	});

	it.each([undefined, null, '', 'yesterday', 42])(
		'ignores a missing or malformed header: %j',
		(header) => {
			recordServerDate('Mon, 23 Nov 2026 10:20:00 GMT');
			const before = serverOffsetMs();

			recordServerDate(header);

			expect(serverOffsetMs()).toBe(before);
		},
	);
});

describe('installServerClock', () => {
	const respond =
		(status: number, date: string): AxiosAdapter =>
		async (config) => {
			const response = {
				data: {},
				status,
				statusText: '',
				headers: { date },
				config,
			};
			if (status >= 400) {
				throw new AxiosError(
					'failed',
					AxiosError.ERR_BAD_RESPONSE,
					config,
					null,
					response,
				);
			}
			return response;
		};

	it('learns the offset from a successful response', async () => {
		const instance = axios.create({
			adapter: respond(200, new Date(BROWSER_NOW + FIVE_MIN).toUTCString()),
		});
		installServerClock(instance);

		await instance.get('/v1/status');

		expect(Math.abs(serverNow() - (BROWSER_NOW + FIVE_MIN))).toBeLessThan(1000);
	});

	it('learns the offset from an error response too', async () => {
		const instance = axios.create({
			adapter: respond(503, new Date(BROWSER_NOW - FIVE_MIN).toUTCString()),
		});
		installServerClock(instance);

		await expect(instance.get('/v1/status')).rejects.toBeDefined();

		expect(Math.abs(serverNow() - (BROWSER_NOW - FIVE_MIN))).toBeLessThan(1000);
	});
});
