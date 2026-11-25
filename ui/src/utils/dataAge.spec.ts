import { describe, expect, it } from 'vitest';

import {
	ageSeconds,
	durationSeconds,
	formatAge,
	isStaleAge,
	STALE_AFTER_SECONDS,
	STATUS_INTERVAL_SECONDS,
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
