import { describe, expect, it } from 'vitest';

import {
	ageSeconds,
	durationSeconds,
	formatAge,
	isStaleAge,
	STALE_AFTER_SECONDS,
	STATUS_INTERVAL_SECONDS,
	staleAfterSeconds,
} from './dataAge';

const T = '2026-11-23T10:00:00Z';
const at = (seconds: number) => Date.parse(T) + seconds * 1000;

describe('ageSeconds', () => {
	it('is the time since observe_time', () => {
		expect(ageSeconds(T, at(7))).toBe(7);
	});

	it('is null without observe_time or with garbage', () => {
		expect(ageSeconds(undefined, at(1))).toBeNull();
		expect(ageSeconds('nope', at(1))).toBeNull();
	});

	it('is clamped at 0 when the browser clock is behind the server', () => {
		expect(ageSeconds(T, at(-30))).toBe(0);
	});
});

describe('isStaleAge', () => {
	it('uses 3 x the status interval', () => {
		expect(STALE_AFTER_SECONDS).toBe(3 * STATUS_INTERVAL_SECONDS);
	});

	it('is false up to the limit and true past it', () => {
		expect(isStaleAge(STALE_AFTER_SECONDS)).toBe(false);
		expect(isStaleAge(STALE_AFTER_SECONDS + 1)).toBe(true);
	});

	it('treats unknown age as stale', () => {
		expect(isStaleAge(null)).toBe(true);
	});

	it('takes the threshold of the status when given', () => {
		expect(isStaleAge(20, 30)).toBe(false);
		expect(isStaleAge(30, 30)).toBe(false);
		expect(isStaleAge(31, 30)).toBe(true);
		expect(isStaleAge(null, 30)).toBe(true);
	});
});

describe('staleAfterSeconds', () => {
	it('is 3 x (interval + cycle_duration) of the status', () => {
		expect(staleAfterSeconds({ interval: '10s', cycle_duration: '2s' })).toBe(
			36,
		);
		expect(staleAfterSeconds({ interval: '5s', cycle_duration: '0.5s' })).toBe(
			16.5,
		);
	});

	it('counts a missing cycle_duration as 0', () => {
		expect(staleAfterSeconds({ interval: '10s' })).toBe(30);
	});

	it('falls back to the default without an interval', () => {
		expect(staleAfterSeconds({ cycle_duration: '2s' })).toBe(
			STALE_AFTER_SECONDS,
		);
		expect(staleAfterSeconds({})).toBe(STALE_AFTER_SECONDS);
		expect(staleAfterSeconds(undefined)).toBe(STALE_AFTER_SECONDS);
		expect(staleAfterSeconds({ interval: 'soon' })).toBe(STALE_AFTER_SECONDS);
	});

	it('marks the boundary: fresh at the threshold, stale one second past', () => {
		const limit = staleAfterSeconds({ interval: '10s', cycle_duration: '2s' });
		expect(isStaleAge(limit, limit)).toBe(false);
		expect(isStaleAge(limit + 1, limit)).toBe(true);
	});
});

describe('formatAge', () => {
	it.each([
		[0, { unit: 's', value: 0 }],
		[59, { unit: 's', value: 59 }],
		[60, { unit: 'm', value: 1 }],
		[3599, { unit: 'm', value: 59 }],
		[3600, { unit: 'h', value: 1 }],
		[90000, { unit: 'h', value: 25 }],
	])('%i s', (seconds, expected) => {
		expect(formatAge(seconds)).toEqual(expected);
	});
});

describe('durationSeconds', () => {
	it('reads a protobuf duration', () => {
		expect(durationSeconds('4.100s')).toBe(4);
		expect(durationSeconds('120s')).toBe(120);
	});

	it('is null for a missing or malformed duration', () => {
		expect(durationSeconds(undefined)).toBeNull();
		expect(durationSeconds('soon')).toBeNull();
	});
});
