import { describe, expect, it } from 'vitest';

import { encodeBasicToken } from './basicAuth';

describe('encodeBasicToken', () => {
	it('encodes name:password as base64', () => {
		expect(encodeBasicToken('Aladdin', 'open sesame')).toBe(
			'QWxhZGRpbjpvcGVuIHNlc2FtZQ==',
		);
	});

	it('encodes non-latin1 credentials as UTF-8 instead of throwing', () => {
		const token = encodeBasicToken('імʼя', 'пароль');
		const decoded = new TextDecoder().decode(
			Uint8Array.from(atob(token), (c) => c.charCodeAt(0)),
		);

		expect(decoded).toBe('імʼя:пароль');
	});

	it('keeps a colon in the password', () => {
		expect(atob(encodeBasicToken('user', 'pa:ss'))).toBe('user:pa:ss');
	});
});
