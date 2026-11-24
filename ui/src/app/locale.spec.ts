import { describe, expect, it } from 'vitest';

import { detectLocale } from './locale';

describe('detectLocale', () => {
	it.each([
		[['uk-UA', 'en-US'], 'uk'],
		[['en-GB'], 'en'],
		[['ru-RU', 'uk'], 'uk'],
		[['de-DE'], 'en'],
		[[], 'en'],
	] as const)('%j -> %s', (languages, expected) => {
		expect(detectLocale(languages)).toBe(expected);
	});
});
