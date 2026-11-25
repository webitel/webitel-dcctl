import {
	afterAll,
	afterEach,
	beforeAll,
	beforeEach,
	describe,
	expect,
	it,
} from 'vitest';

import { watchOperation } from '@/api/client/watchStream';
import type {
	WebitelDcctlV1ListOperationsResponse as List,
	WebitelDcctlV1Operation as Operation,
} from '@/api/gen/_models';
import { encodeBasicToken } from '@/utils/basicAuth';
import { MOCK_USER } from './handlers';
import {
	buildOperation,
	DROP_AFTER_MESSAGES,
	MOCK_OPERATION_IDS,
	resetScenarios,
	setOperationsVariant,
} from './operationVariants';
import { server } from './server';

const AUTH = `Basic ${encodeBasicToken(MOCK_USER.name, MOCK_USER.password)}`;
const API = 'http://localhost/dcctl/api';

const get = (path: string) =>
	fetch(`${API}${path}`, { headers: { Authorization: AUTH } });

const watchAll = async (id: string, signal = new AbortController().signal) => {
	const seen: Operation[] = [];
	await watchOperation(id, {
		authorization: AUTH,
		signal,
		onOperation: (operation) => seen.push(operation),
		fetchImpl: (url, init) => fetch(`http://localhost${String(url)}`, init),
	});
	return seen;
};

describe('operation fixtures', () => {
	it('carry no explicit zero: the gateway omits it', () => {
		const ids = Object.values(MOCK_OPERATION_IDS);
		const operations = ids.map((id) => buildOperation(id) as Operation);
		for (const operation of operations) {
			for (const step of operation.steps ?? []) {
				expect(step.index).not.toBe(0);
				expect(step.stage).not.toBe(0);
			}
			expect(operation.previous_generation).not.toBe('0');
		}
		expect(
			operations.some((op) =>
				op.steps?.some((step) => step.index === undefined),
			),
		).toBe(true);
	});

	it('include a PATRONI_FAILOVER step and a forced mode', () => {
		const operation = buildOperation(MOCK_OPERATION_IDS.failover) as Operation;
		expect(
			operation.steps?.some((step) => step.type === 'PATRONI_FAILOVER'),
		).toBe(true);
		expect(operation.mode).toBe('FORCED');
	});

	it('have one point of no return in a switch', () => {
		const operation = buildOperation(MOCK_OPERATION_IDS.running) as Operation;
		expect(
			operation.steps?.filter((step) => step.point_of_no_return),
		).toHaveLength(1);
	});

	it('have states for every screen state: running, failed, interrupted, succeeded', () => {
		const states = [
			MOCK_OPERATION_IDS.running,
			MOCK_OPERATION_IDS.failed,
			MOCK_OPERATION_IDS.interrupted,
			MOCK_OPERATION_IDS.failover,
		].map((id) => buildOperation(id)?.state);
		expect(states).toEqual(['RUNNING', 'FAILED', 'INTERRUPTED', 'SUCCEEDED']);
	});

	it('know nothing about another id', () => {
		expect(buildOperation('nope')).toBeUndefined();
	});
});

describe('operation handlers', () => {
	beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
	beforeEach(() => resetScenarios(5));
	afterEach(() => {
		server.resetHandlers();
		setOperationsVariant('normal');
	});
	afterAll(() => server.close());

	it('answer GetOperation with the operation and 404 with an API error for an unknown id', async () => {
		const found = await get(`/v1/operations/${MOCK_OPERATION_IDS.failover}`);
		expect(((await found.json()) as Operation).type).toBe('FAILOVER');

		const missing = await get('/v1/operations/nope');
		expect(missing.status).toBe(404);
		expect(await missing.json()).toMatchObject({
			id: 'dcctl.operation.not_found',
		});
	});

	it('require the credentials for the watch like for any other call', async () => {
		const response = await fetch(
			`${API}/v1/operations/${MOCK_OPERATION_IDS.failover}:watch`,
		);
		expect(response.status).toBe(401);
	});

	it('page the list with page_token until the last page', async () => {
		setOperationsVariant('many');
		const first = (await (
			await get('/v1/operations?page_size=50')
		).json()) as List;
		expect(first.operations).toHaveLength(50);
		expect(first.next_page_token).toBeTruthy();
		const second = (await (
			await get(
				`/v1/operations?page_size=50&page_token=${first.next_page_token}`,
			)
		).json()) as List;
		const third = (await (
			await get(
				`/v1/operations?page_size=50&page_token=${second.next_page_token}`,
			)
		).json()) as List;
		expect(third.operations).toHaveLength(20);
		expect(third.next_page_token).toBeUndefined();
		const names = [first, second, third]
			.flatMap((page) => page.operations ?? [])
			.map((op) => op.name);
		expect(new Set(names).size).toBe(120);
	});

	it('answer an empty list without a token, and 503 for the unavailable variant', async () => {
		setOperationsVariant('empty');
		expect(await (await get('/v1/operations')).json()).toEqual({
			operations: [],
		});
		setOperationsVariant('unavailable');
		expect((await get('/v1/operations')).status).toBe(503);
		expect((await get('/v1/operations/x')).status).toBe(503);
	});

	it('stream the first message at once, several step changes, then the final state, and close', async () => {
		const seen = await watchAll(MOCK_OPERATION_IDS.progress);
		expect(seen.length).toBeGreaterThan(3);
		expect(seen[0]?.state).toBe('RUNNING');
		expect(seen[seen.length - 1]?.state).toBe('SUCCEEDED');
		const runningStep = (op: Operation) =>
			op.steps?.findIndex((step) => step.state === 'RUNNING');
		expect(runningStep(seen[1] as Operation)).toBeGreaterThan(
			runningStep(seen[0] as Operation) as number,
		);
	});

	it('break the first connection of watch-drop and play out the next one', async () => {
		const first: Operation[] = [];
		await expect(
			watchOperation(MOCK_OPERATION_IDS.drop, {
				authorization: AUTH,
				signal: new AbortController().signal,
				onOperation: (operation) => first.push(operation),
				fetchImpl: (url, init) => fetch(`http://localhost${String(url)}`, init),
			}),
		).rejects.toBeDefined();
		expect(first).toHaveLength(DROP_AFTER_MESSAGES);

		const second = await watchAll(MOCK_OPERATION_IDS.drop);
		expect(second[second.length - 1]?.state).toBe('SUCCEEDED');
	});

	it('keep a running fixture open until the client aborts', async () => {
		const controller = new AbortController();
		const seen: Operation[] = [];
		const promise = watchOperation(MOCK_OPERATION_IDS.running, {
			authorization: AUTH,
			signal: controller.signal,
			onOperation: (operation) => seen.push(operation),
			fetchImpl: (url, init) => fetch(`http://localhost${String(url)}`, init),
		});
		await new Promise((resolve) => setTimeout(resolve, 40));
		expect(seen).toHaveLength(1);
		controller.abort();
		await expect(promise).rejects.toMatchObject({ name: 'AbortError' });
	});

	it('answer 404 for the watch of an unknown id', async () => {
		await expect(watchAll('nope')).rejects.toMatchObject({ status: 404 });
	});
});
