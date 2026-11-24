/** Base64 of `name:password` in UTF-8 (RFC 7617), without the `Basic ` prefix. */
export const encodeBasicToken = (name: string, password: string): string => {
	const bytes = new TextEncoder().encode(`${name}:${password}`);
	return btoa(String.fromCharCode(...bytes));
};
