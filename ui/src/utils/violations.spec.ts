import { describe, expect, it } from 'vitest';

import { formatViolations } from './violations';

describe('formatViolations', () => {
	it('keeps type, subject and text', () => {
		expect(
			formatViolations([
				{ type: 'check', subject: 'coord.lock_free', description: 'busy' },
			]),
		).toEqual([{ type: 'check', subject: 'coord.lock_free', text: 'busy' }]);
	});

	it('drops violations without a description', () => {
		expect(
			formatViolations([{ subject: 'a' }, { subject: 'b', description: ' ' }]),
		).toEqual([]);
	});

	it('keeps the line breaks of a parser error and trims the tail', () => {
		const text = 'unexpected token\ntype = "A" AND\n             ^\n';
		const [first] = formatViolations([
			{ type: 'field', subject: 'filter', description: text },
		]);
		expect(first.text).toBe('unexpected token\ntype = "A" AND\n             ^');
	});

	it('handles a missing list', () => {
		expect(formatViolations(undefined)).toEqual([]);
	});
});
