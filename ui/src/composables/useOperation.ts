import type { MaybeRefOrGetter } from 'vue';
import { computed, onScopeDispose, ref, shallowRef, toValue, watch } from 'vue';

import { getApiError, isUnauthorized } from '@/api/client/apiError';
import { WatchHttpError, watchOperation } from '@/api/client/watchStream';
import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { getOperationService } from '@/api/gen/operation-service/operation-service';
import { useAuthStore } from '@/stores/auth';
import { isOlderOperation, isTerminalState } from '@/utils/operationView';

const NOT_FOUND_ID = 'dcctl.operation.not_found';
/** A read that hangs must not stop the recovery. */
const READ_TIMEOUT_MS = 10_000;
const CLOCK_TICK_MS = 1000;
const FIRST_DELAY_MS = 1000;
const MAX_DELAY_MS = 10_000;

/** Pause before the next attempt after `failures` failed or ended attempts in a row. */
export const reconnectDelayMs = (failures: number): number =>
	Math.min(FIRST_DELAY_MS * 2 ** failures, MAX_DELAY_MS);

/**
 * `connecting`: no message of the stream yet. `live`: the stream delivers.
 * `reconnecting`: it dropped; the data on screen is as old as its age says.
 * `ended`: nothing more to come (final state, or the login ended).
 */
export type Connection = 'connecting' | 'live' | 'reconnecting' | 'ended';

const sleep = (ms: number, signal: AbortSignal) =>
	new Promise<void>((resolve) => {
		const timer = setTimeout(resolve, ms);
		signal.addEventListener(
			'abort',
			() => {
				clearTimeout(timer);
				resolve();
			},
			{ once: true },
		);
	});

const isUnauthorizedWatch = (cause: unknown) =>
	cause instanceof WatchHttpError && cause.status === 401;

const isNotFound = (cause: unknown) =>
	getApiError(cause)?.id === NOT_FOUND_ID ||
	(cause instanceof WatchHttpError && cause.apiErrorId === NOT_FOUND_ID);

/**
 * One operation, kept current from `WatchOperation`. It reads the operation,
 * then follows the stream. After a drop it reads the operation again before it
 * watches again, so a change (or the final state) missed in between is not
 * lost, and it waits a little longer after each failure. It stops at a final
 * state, on 401, when the operation does not exist, and when the scope ends or
 * the id changes (the stream and the read in flight are aborted, and what they
 * deliver late is dropped).
 */
export const useOperation = (id: MaybeRefOrGetter<string>) => {
	const auth = useAuthStore();
	const operation = shallowRef<Operation>();
	/** Last failure; `null` while everything works. */
	const error = ref<unknown>(null);
	const notFound = ref(false);
	const connection = ref<Connection>('connecting');
	/** Browser time of the last message that updated the data. */
	const receivedAt = ref<number | null>(null);
	const now = ref(Date.now());

	// Bumped by every stop: a late answer of an older session is dropped.
	let epoch = 0;
	let attempt: AbortController | null = null;
	let restart = false;
	let clock: ReturnType<typeof setInterval> | undefined;

	/** Takes `next` unless it is an older version than the one shown. */
	const apply = (next: Operation) => {
		const current = operation.value;
		if (current && isOlderOperation(next, current)) return;
		operation.value = next;
		receivedAt.value = Date.now();
		error.value = null;
	};

	const run = async (session: number, operationId: string) => {
		const isCurrent = () => session === epoch;
		let failures = 0;
		let finished = false;
		while (isCurrent()) {
			const controller = new AbortController();
			attempt = controller;
			const authorization = auth.authorization;
			try {
				const read = await getOperationService().getOperation(
					encodeURIComponent(operationId),
					{ timeout: READ_TIMEOUT_MS, signal: controller.signal },
				);
				if (!isCurrent()) return;
				apply(read);
				if (isTerminalState(operation.value?.state)) {
					connection.value = 'ended';
					return;
				}
				await watchOperation(operationId, {
					authorization,
					signal: controller.signal,
					onOperation: (message) => {
						if (!isCurrent() || controller.signal.aborted) return;
						apply(message);
						connection.value = 'live';
						failures = 0;
						if (isTerminalState(operation.value?.state)) {
							finished = true;
							controller.abort();
						}
					},
				});
				// The server closed the stream without a final state.
			} catch (cause) {
				if (!isCurrent()) return;
				if (finished) {
					connection.value = 'ended';
					return;
				}
				if (restart) {
					restart = false;
					continue;
				}
				if (isNotFound(cause)) {
					operation.value = undefined;
					notFound.value = true;
					connection.value = 'ended';
					return;
				}
				if (isUnauthorized(cause) || isUnauthorizedWatch(cause)) {
					// A refusal of an older login must not end the new one.
					if (authorization === auth.authorization) auth.clear();
					connection.value = 'ended';
					return;
				}
				error.value = cause;
			}
			if (!isCurrent()) return;
			if (operation.value) connection.value = 'reconnecting';
			await sleep(reconnectDelayMs(failures++), controller.signal);
			if (restart) {
				restart = false;
				failures = 0;
			}
		}
	};

	const stop = () => {
		epoch++;
		restart = false;
		attempt?.abort();
		attempt = null;
		clearInterval(clock);
		clock = undefined;
	};

	const start = (operationId: string) => {
		stop();
		operation.value = undefined;
		error.value = null;
		notFound.value = false;
		receivedAt.value = null;
		connection.value = 'connecting';
		now.value = Date.now();
		clock = setInterval(() => {
			now.value = Date.now();
		}, CLOCK_TICK_MS);
		void run(epoch, operationId);
	};

	watch(() => toValue(id), start, { immediate: true });
	onScopeDispose(stop);

	/** Aborts the attempt and starts a new one at once: read, then watch. */
	const reconnect = () => {
		if (!attempt || attempt.signal.aborted) return;
		restart = true;
		attempt.abort();
	};

	const phase = computed<'loading' | 'ready' | 'notFound' | 'error'>(() => {
		if (notFound.value) return 'notFound';
		if (operation.value) return 'ready';
		return error.value ? 'error' : 'loading';
	});

	/** Seconds since the data was last confirmed by the server; `null` without data. */
	const age = computed(() =>
		receivedAt.value === null
			? null
			: Math.max(0, Math.floor((now.value - receivedAt.value) / 1000)),
	);

	return {
		operation,
		phase,
		error,
		connection,
		now,
		age,
		reconnect,
	};
};
