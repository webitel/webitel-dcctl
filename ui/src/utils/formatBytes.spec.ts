import { describe, expect, it } from 'vitest';

import { formatBytes } from './formatBytes';

describe('formatBytes', () => {
	it.each([
		['0', '0 byte'],
		['512', '512 byte'],
		['1024', '1 kB'],
		['1572864', '1.5 MB'],
		['1073741824', '1 GB'],
		['5497558138880', '5 TB'],
	])('%s -> %s in English', (bytes, expected) => {
		expect(formatBytes(bytes, 'en')).toBe(expected);
	});

	it('uses the number format of the locale', () => {
		expect(formatBytes('1572864', 'uk')).toContain('1,5');
	});

	it('accepts a number', () => {
		expect(formatBytes(2048, 'en')).toBe('2 kB');
	});

	it.each([undefined, '', 'many', '-1'])('is empty for %j', (value) => {
		expect(formatBytes(value, 'en')).toBe('');
	});
});
