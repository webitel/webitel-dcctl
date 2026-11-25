import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';

import { API_BASE } from './apiBase';
import { recordServerDate } from './serverClock';

/** The response did not start within this time: the connection is treated as dead. */
export const WATCH_CONNECT_TIMEOUT_MS = 10_000;

/** The server answered the watch request with an HTTP error. */
export class WatchHttpError extends Error {
	readonly status: number;
	/** `id` of the API error body, when the body is one. */
	readonly apiErrorId: string | undefined;

	constructor(status: number, apiErrorId?: string) {
		super(`watch failed with status ${status}`);
		this.name = 'WatchHttpError';
		this.status = status;
		this.apiErrorId = apiErrorId;
	}
}

/** The stream sent something that is not an operation message. */
export class WatchProtocolError extends Error {
	constructor(message: string) {
		super(message);
		this.name = 'WatchProtocolError';
	}
}

const isObject = (value: unknown): value is Record<string, unknown> =>
	typeof value === 'object' && value !== null && !Array.isArray(value);

const parseLine = (text: string): Operation => {
	let message: unknown;
	try {
		message = JSON.parse(text);
	} catch {
		throw new WatchProtocolError('a line of the stream is not JSON');
	}
	if (!isObject(message)) {
		throw new WatchProtocolError('a line of the stream is not an object');
	}
	if (isObject(message.error)) {
		throw new WatchProtocolError(
			`the stream failed: ${String(message.error.message ?? 'unknown error')}`,
		);
	}
	if (!isObject(message.result)) {
		throw new WatchProtocolError('a line of the stream has no result');
	}
	return message.result as Operation;
};

export interface WatchParser {
	/** Feeds text; returns the operations of the lines completed by it. */
	push: (text: string) => Operation[];
	/** Returns the operation of a last line the server closed without a newline. */
	end: () => Operation[];
}

/**
 * Splits the stream into lines (one `{"result": operation}` each). A line
 * that cannot be read throws instead of being skipped: it may have carried
 * the change that matters, so the caller reads the operation again.
 */
export const createWatchParser = (): WatchParser => {
	let rest = '';
	const read = (lines: string[]): Operation[] =>
		lines
			.map((text) => text.trim())
			.filter((text) => text !== '')
			.map(parseLine);
	return {
		push: (text) => {
			const lines = (rest + text).split('\n');
			rest = lines.pop() ?? '';
			return read(lines);
		},
		end: () => {
			const last = rest;
			rest = '';
			return read([last]);
		},
	};
};

export interface WatchOptions {
	/** `Authorization` header value; `null` when logged out. */
	authorization: string | null;
	/** Aborts the connection and the reading. */
	signal: AbortSignal;
	onOperation: (operation: Operation) => void;
	connectTimeoutMs?: number;
	fetchImpl?: typeof fetch;
}

const abortError = () =>
	new DOMException('The watch was aborted', 'AbortError');

const errorIdOf = async (response: Response): Promise<string | undefined> => {
	try {
		const body: unknown = await response.json();
		return isObject(body) && typeof body.id === 'string' ? body.id : undefined;
	} catch {
		return undefined;
	}
};

/**
 * Reads `GET /v1/operations/{id}:watch` (one JSON line per message) with
 * `fetch`: the generated axios call cannot stream and the browser's
 * `EventSource` cannot send `Authorization`. Resolves when the server closes
 * the stream; rejects on an HTTP error, a broken or unreadable stream, a
 * timeout while connecting, or an abort. The stream has no timeout once it
 * has started, since a long step sends nothing.
 */
export const watchOperation = async (
	id: string,
	{
		authorization,
		signal,
		onOperation,
		connectTimeoutMs = WATCH_CONNECT_TIMEOUT_MS,
		fetchImpl = fetch,
	}: WatchOptions,
): Promise<void> => {
	if (signal.aborted) throw abortError();
	const controller = new AbortController();
	const forwardAbort = () => controller.abort();
	signal.addEventListener('abort', forwardAbort, { once: true });
	const timer = setTimeout(
		() => controller.abort(new Error('the watch did not start in time')),
		connectTimeoutMs,
	);
	try {
		const headers: Record<string, string> = { Accept: 'application/json' };
		if (authorization) headers.Authorization = authorization;
		const response = await fetchImpl(
			`${API_BASE}/v1/operations/${encodeURIComponent(id)}:watch`,
			{ headers, signal: controller.signal },
		);
		clearTimeout(timer);
		recordServerDate(response.headers.get('date'));
		if (!response.ok) {
			throw new WatchHttpError(response.status, await errorIdOf(response));
		}
		if (!response.body)
			throw new WatchProtocolError('the response has no body');

		const reader = response.body.getReader();
		const cancel = () => void reader.cancel().catch(() => undefined);
		controller.signal.addEventListener('abort', cancel, { once: true });
		const decoder = new TextDecoder();
		const parser = createWatchParser();
		for (;;) {
			const { done, value } = await reader.read();
			if (signal.aborted) throw abortError();
			if (done) break;
			for (const operation of parser.push(
				decoder.decode(value, { stream: true }),
			)) {
				onOperation(operation);
			}
		}
		const tail = decoder.decode();
		for (const operation of [...parser.push(tail), ...parser.end()]) {
			onOperation(operation);
		}
	} finally {
		clearTimeout(timer);
		signal.removeEventListener('abort', forwardAbort);
	}
};
