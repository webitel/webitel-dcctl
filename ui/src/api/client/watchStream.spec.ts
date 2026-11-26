import { afterEach, describe, expect, it, vi } from 'vitest';

import { resetServerClock, serverOffsetMs } from './serverClock';
import {
	createWatchParser,
	WatchHttpError,
	WatchProtocolError,
	watchOperation,
} from './watchStream';

const line = (result: unknown) => `${JSON.stringify({ result })}\n`;

describe('createWatchParser', () => {
	it('returns one operation per line', () => {
		const parser = createWatchParser();
		expect(
			parser.push(line({ name: 'operations/a', state: 'RUNNING' })),
		).toEqual([{ name: 'operations/a', state: 'RUNNING' }]);
	});

	it('returns several operations of one chunk in order', () => {
		const parser = createWatchParser();
		const chunk =
			line({ name: 'a', state: 'RUNNING' }) +
			line({ name: 'a', state: 'FAILED' });
		expect(parser.push(chunk).map((op) => op.state)).toEqual([
			'RUNNING',
			'FAILED',
		]);
	});

	it('waits for the end of a line split between chunks', () => {
		const parser = createWatchParser();
		const text = line({ name: 'a', state: 'SUCCEEDED' });
		expect(parser.push(text.slice(0, 12))).toEqual([]);
		expect(parser.push(text.slice(12, 30))).toEqual([]);
		expect(parser.push(text.slice(30))).toEqual([
			{ name: 'a', state: 'SUCCEEDED' },
		]);
	});

	it('handles a chunk that ends in the middle of a line after whole lines', () => {
		const parser = createWatchParser();
		const second = line({ name: 'a', state: 'FAILED' });
		const first = parser.push(
			line({ name: 'a', state: 'RUNNING' }) + second.slice(0, 5),
		);
		expect(first.map((op) => op.state)).toEqual(['RUNNING']);
		expect(parser.push(second.slice(5)).map((op) => op.state)).toEqual([
			'FAILED',
		]);
	});

	it('ignores blank lines and accepts CRLF', () => {
		const parser = createWatchParser();
		const text = `\n${JSON.stringify({ result: { name: 'a' } })}\r\n\n`;
		expect(parser.push(text)).toEqual([{ name: 'a' }]);
	});

	it('does not drop a last line without the newline once the stream ends', () => {
		const parser = createWatchParser();
		expect(parser.push(JSON.stringify({ result: { name: 'a' } }))).toEqual([]);
		expect(parser.end()).toEqual([{ name: 'a' }]);
	});

	it('fails on a malformed line instead of skipping it', () => {
		const parser = createWatchParser();
		expect(() => parser.push('{"result": {"name"\n')).toThrow(
			WatchProtocolError,
		);
	});

	it.each([
		['a line that is not an object', '[1]\n'],
		['an object without result or error', '{"other": 1}\n'],
		['a result that is not an object', '{"result": 5}\n'],
	])('fails on %s', (_name, text) => {
		expect(() => createWatchParser().push(text)).toThrow(WatchProtocolError);
	});

	it('fails with the detail and the id of a gateway error line', () => {
		const text = `${JSON.stringify({ error: { id: 'dcctl.leader.unavailable', code: 503, detail: 'the leader does not answer' } })}\n`;
		expect(() => createWatchParser().push(text)).toThrow(
			/the leader does not answer/,
		);
		expect(() => createWatchParser().push(text)).toThrow(
			expect.objectContaining({ apiErrorId: 'dcctl.leader.unavailable' }),
		);
	});

	it('says unknown error for an error line without a detail', () => {
		expect(() => createWatchParser().push('{"error":{"code":14}}\n')).toThrow(
			/unknown error/,
		);
	});

	it('keeps the operations that came before a malformed line out of the failure', () => {
		const parser = createWatchParser();
		const received: unknown[] = [];
		try {
			received.push(...parser.push(line({ name: 'a' })));
			parser.push('garbage\n');
		} catch {
			// the stream is dead; the operation before it was already delivered
		}
		expect(received).toEqual([{ name: 'a' }]);
	});
});

const streamOf = (...chunks: string[]) => {
	const encoder = new TextEncoder();
	return new ReadableStream<Uint8Array>({
		start(controller) {
			for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
			controller.close();
		},
	});
};

const respond = (
	body: ReadableStream<Uint8Array> | string,
	init: ResponseInit = {},
) => new Response(body, { status: 200, ...init });

describe('watchOperation', () => {
	afterEach(() => {
		resetServerClock();
		vi.useRealTimers();
	});

	const run = (fetchImpl: typeof fetch, extra: object = {}) => {
		const seen: unknown[] = [];
		const promise = watchOperation('0b9f', {
			authorization: 'Basic abc',
			signal: new AbortController().signal,
			onOperation: (op) => seen.push(op),
			fetchImpl,
			...extra,
		});
		return { seen, promise };
	};

	it('calls the watch endpoint under the API base with the Authorization header', async () => {
		const fetchImpl = vi.fn(async () => respond(streamOf(line({ name: 'a' }))));
		await run(fetchImpl as never).promise;
		const [url, init] = fetchImpl.mock.calls[0] as unknown as [
			string,
			RequestInit,
		];
		expect(url).toBe('/dcctl/api/v1/operations/0b9f:watch');
		expect((init.headers as Record<string, string>).Authorization).toBe(
			'Basic abc',
		);
	});

	it('delivers operations as they arrive and resolves when the server closes the stream', async () => {
		const fetchImpl = async () =>
			respond(
				streamOf(
					line({ state: 'RUNNING' }).slice(0, 9),
					`${line({ state: 'RUNNING' }).slice(9)}${line({ state: 'SUCCEEDED' })}`,
				),
			);
		const { seen, promise } = run(fetchImpl as never);
		await promise;
		expect(seen).toEqual([{ state: 'RUNNING' }, { state: 'SUCCEEDED' }]);
	});

	it('escapes the operation id so it stays one path segment', async () => {
		const fetchImpl = vi.fn(async () => respond(streamOf()));
		await watchOperation('a/b', {
			authorization: null,
			signal: new AbortController().signal,
			onOperation: () => undefined,
			fetchImpl: fetchImpl as never,
		});
		expect((fetchImpl.mock.calls[0] as unknown[])[0]).toBe(
			'/dcctl/api/v1/operations/a%2Fb:watch',
		);
	});

	it('sends no Authorization header when logged out', async () => {
		const fetchImpl = vi.fn(async () => respond(streamOf()));
		await watchOperation('a', {
			authorization: null,
			signal: new AbortController().signal,
			onOperation: () => undefined,
			fetchImpl: fetchImpl as never,
		});
		const init = (
			fetchImpl.mock.calls[0] as unknown as [string, RequestInit]
		)[1];
		expect(init.headers).not.toHaveProperty('Authorization');
	});

	it('records the server clock from the Date header', async () => {
		const date = new Date(Date.now() + 120_000).toUTCString();
		await run((async () =>
			respond(streamOf(), { headers: { Date: date } })) as never).promise;
		expect(serverOffsetMs()).toBeGreaterThan(110_000);
	});

	it('rejects with the status and the API error id of a failed response', async () => {
		const body = JSON.stringify({
			id: 'dcctl.operation.not_found',
			code: 404,
			detail: 'x',
		});
		const { promise } = run((async () =>
			respond(body, { status: 404 })) as never);
		await expect(promise).rejects.toMatchObject({
			name: 'WatchHttpError',
			status: 404,
			apiErrorId: 'dcctl.operation.not_found',
			message: 'x',
		});
		await expect(promise).rejects.toBeInstanceOf(WatchHttpError);
	});

	it('rejects with the status when the body of a failure is not an API error', async () => {
		const { promise } = run((async () =>
			respond('<html>', { status: 502 })) as never);
		await expect(promise).rejects.toMatchObject({
			status: 502,
			apiErrorId: undefined,
		});
	});

	it('rejects when the stream breaks in the middle', async () => {
		let pulls = 0;
		const stream = new ReadableStream<Uint8Array>({
			pull(controller) {
				if (pulls++ === 0) {
					controller.enqueue(
						new TextEncoder().encode(line({ state: 'RUNNING' })),
					);
				} else {
					controller.error(new TypeError('network error'));
				}
			},
		});
		const { seen, promise } = run((async () => respond(stream)) as never);
		await expect(promise).rejects.toThrow('network error');
		expect(seen).toEqual([{ state: 'RUNNING' }]);
	});

	it.each([
		['a malformed line', 'nonsense\n', undefined],
		[
			'a handler that throws',
			`${line({ state: 'RUNNING' })}`,
			new Error('handler failed'),
		],
	])('closes the body after %s', async (_name, chunk, handlerError) => {
		const cancel = vi.fn();
		const stream = new ReadableStream<Uint8Array>({
			start(controller) {
				controller.enqueue(new TextEncoder().encode(chunk));
			},
			cancel,
		});
		const { promise } = run((async () => respond(stream)) as never, {
			onOperation: () => {
				if (handlerError) throw handlerError;
			},
		});
		await expect(promise).rejects.toBeDefined();
		expect(cancel).toHaveBeenCalled();
	});

	it('rejects with the detail of an error line after the first message', async () => {
		const first = JSON.stringify({ result: { name: 'operations/x' } });
		const failure = JSON.stringify({
			error: { id: 'dcctl.lock.unavailable', code: 503, detail: 'no consul' },
		});
		const { promise, seen } = run((async () =>
			respond(
				streamOf(
					`${first}
`,
					`${failure}
`,
				),
			)) as never);
		await expect(promise).rejects.toMatchObject({
			name: 'WatchProtocolError',
			apiErrorId: 'dcctl.lock.unavailable',
			message: expect.stringContaining('no consul'),
		});
		expect(seen).toHaveLength(1);
	});

	it('rejects on a malformed line', async () => {
		const { promise } = run((async () =>
			respond(streamOf('nonsense\n'))) as never);
		await expect(promise).rejects.toBeInstanceOf(WatchProtocolError);
	});

	it('gives up when the response does not start in time', async () => {
		vi.useFakeTimers();
		const fetchImpl = (_url: string, init: RequestInit) =>
			new Promise<Response>((_resolve, reject) => {
				init.signal?.addEventListener('abort', () =>
					reject(init.signal?.reason),
				);
			});
		const { promise } = run(fetchImpl as never, { connectTimeoutMs: 1000 });
		const assertion = expect(promise).rejects.toBeDefined();
		await vi.advanceTimersByTimeAsync(1001);
		await assertion;
	});

	it('does not time out a stream that has started and is quiet', async () => {
		vi.useFakeTimers();
		let closeStream: () => void = () => undefined;
		const stream = new ReadableStream<Uint8Array>({
			start(controller) {
				controller.enqueue(
					new TextEncoder().encode(line({ state: 'RUNNING' })),
				);
				closeStream = () => controller.close();
			},
		});
		const { seen, promise } = run((async () => respond(stream)) as never, {
			connectTimeoutMs: 1000,
		});
		await vi.advanceTimersByTimeAsync(60_000);
		closeStream();
		await promise;
		expect(seen).toEqual([{ state: 'RUNNING' }]);
	});

	it('stops reading and rejects when aborted', async () => {
		const controller = new AbortController();
		const stream = new ReadableStream<Uint8Array>({
			start(c) {
				c.enqueue(new TextEncoder().encode(line({ state: 'RUNNING' })));
			},
		});
		const seen: unknown[] = [];
		const promise = watchOperation('a', {
			authorization: null,
			signal: controller.signal,
			onOperation: (op) => seen.push(op),
			fetchImpl: (async (_url: string, init: RequestInit) => {
				init.signal?.addEventListener('abort', () => undefined);
				return respond(stream);
			}) as never,
		});
		await vi.waitFor(() => expect(seen).toHaveLength(1));
		controller.abort();
		await expect(promise).rejects.toMatchObject({ name: 'AbortError' });
	});
});
