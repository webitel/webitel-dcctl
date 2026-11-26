import { describe, expect, it } from 'vitest';

import { formatTimeWithZone } from './timeText';

const INSTANT = '2026-11-26T11:45:10Z';

describe('formatTimeWithZone', () => {
	it.each([
		['Europe/Kyiv', 'UTC+02:00'],
		['Asia/Kolkata', 'UTC+05:30'],
		['America/St_Johns', 'UTC-03:30'],
		['America/New_York', 'UTC-05:00'],
		['UTC', 'UTC'],
	])('labels %s with %s', (zone, label) => {
		expect(formatTimeWithZone(INSTANT, 'en', zone).endsWith(` ${label}`)).toBe(
			true,
		);
	});

	it('shows the wall clock of the zone', () => {
		expect(formatTimeWithZone(INSTANT, 'en-GB', 'Europe/Kyiv')).toBe(
			'26/11/2026, 13:45:10 UTC+02:00',
		);
		expect(formatTimeWithZone(INSTANT, 'en-GB', 'America/St_Johns')).toBe(
			'26/11/2026, 08:15:10 UTC-03:30',
		);
		expect(formatTimeWithZone(INSTANT, 'en-GB', 'UTC')).toBe(
			'26/11/2026, 11:45:10 UTC',
		);
	});

	it('uses the same offset label in every language', () => {
		for (const locale of ['en', 'uk', 'de', 'ar']) {
			expect(formatTimeWithZone(INSTANT, locale, 'America/St_Johns')).toContain(
				' UTC-03:30',
			);
		}
	});

	it('follows daylight saving of the instant', () => {
		expect(
			formatTimeWithZone('2026-07-01T00:00:00Z', 'en', 'Europe/Kyiv'),
		).toContain('UTC+03:00');
	});
});
