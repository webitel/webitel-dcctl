import { isCancel } from 'axios';
import { computed, onScopeDispose, ref } from 'vue';

import { type ActionTarget, callAction } from '@/api/client/action';
import {
	type ApiError,
	getApiError,
	isUnauthorized,
} from '@/api/client/apiError';
import type { WebitelDcctlV1Operation as Operation } from '@/api/gen/_models';
import { isAlwaysTyped } from '@/utils/actionAvailability';
import {
	blockingChecks,
	buildConfirmation,
	type ConfirmationField,
	type ConfirmationNeed,
	needSignature,
	type Preview,
	previewOfOperation,
	previewOfPreflight,
	requiredConfirmations,
} from '@/utils/actionPreview';

/**
 * `choosing`: `resolve` waits for a winner. `checking`: the dry run runs.
 * `ready`: the dry run answered (see `outcome`). `submitting`: the real call runs.
 * `done`: the operation exists (`result`).
 */
export type DialogPhase =
	| 'choosing'
	| 'checking'
	| 'ready'
	| 'submitting'
	| 'done';

/**
 * What the dry run said. `ok`: the action can run. `confirm`: it can run once
 * the confirmations are given. `blocked`: a check rejects it. `refused`: any
 * other refusal (a busy lock, a running operation, 503).
 */
export type DryRunOutcome = 'ok' | 'confirm' | 'blocked' | 'refused';

export interface Refusal {
	error: unknown;
	/** The API error body, `null` when the request failed without one (network). */
	api: ApiError | null;
}

const newRequestId = () => crypto.randomUUID();

/**
 * The flow of the action dialog: a dry run first, then, once the operator has
 * given the confirmations and typed the name, the real call. `targetName` is the
 * name to type for `resume`: the datacenter the operation targets.
 */
export const useActionDialog = (base: ActionTarget, targetName?: string) => {
	const phase = ref<DialogPhase>(
		base.kind === 'resolve' ? 'choosing' : 'checking',
	);
	const preview = ref<Preview | null>(null);
	const outcome = ref<DryRunOutcome | null>(null);
	const refusal = ref<Refusal | null>(null);
	const accepted = ref<ConfirmationField[]>([]);
	const typed = ref('');
	const winner = ref('');
	/** The real call found more to confirm than the dry run showed. */
	const factsChanged = ref(false);
	const result = ref<Operation | null>(null);

	// Generated once per dialog; a new one only when the body changes (`resolve`: another winner).
	let requestId = newRequestId();
	let sequence = 0;
	let controller: AbortController | undefined;

	const target = (): ActionTarget =>
		base.kind === 'resolve' ? { ...base, winner: winner.value } : base;

	const needs = computed<ConfirmationNeed[]>(() =>
		outcome.value === 'confirm' && preview.value
			? requiredConfirmations(preview.value.checks)
			: [],
	);
	/** `blocked`: confirmations that will be asked once the blocking checks pass. Information only. */
	const pendingConfirmations = computed(() =>
		outcome.value === 'blocked' && preview.value
			? requiredConfirmations(preview.value.checks)
			: [],
	);
	const blockers = computed(() =>
		preview.value ? blockingChecks(preview.value.checks) : [],
	);

	const expectedName = computed(() =>
		base.kind === 'resolve'
			? winner.value
			: (base.datacenter ?? targetName ?? ''),
	);
	const typedNameRequired = computed(() => {
		if (isAlwaysTyped(base.kind)) return true;
		if (base.kind === 'repoint') {
			return needs.value.some((need) => need.field === 'max_discard_bytes');
		}
		if (base.kind === 'resume') {
			return (
				preview.value?.mode === 'FORCED' ||
				needs.value.some((need) => need.field === 'isolation')
			);
		}
		return false;
	});
	const typedNameOk = computed(
		() =>
			!typedNameRequired.value ||
			(expectedName.value !== '' && typed.value.trim() === expectedName.value),
	);

	const allAccepted = computed(() =>
		needs.value.every((need) => accepted.value.includes(need.field)),
	);
	const canSubmit = computed(
		() =>
			phase.value === 'ready' &&
			(outcome.value === 'ok' || outcome.value === 'confirm') &&
			blockers.value.length === 0 &&
			allAccepted.value &&
			typedNameOk.value,
	);

	const begin = (next: DialogPhase) => {
		controller?.abort();
		controller = new AbortController();
		sequence += 1;
		phase.value = next;
		refusal.value = null;
		return { id: sequence, signal: controller.signal };
	};

	// What the operator saw when ticking a confirmation: only that is ever sent.
	const acceptedSignature = new Map<ConfirmationField, string>();

	const setAccepted = (field: ConfirmationField, value: boolean) => {
		const rest = accepted.value.filter((item) => item !== field);
		const need = needs.value.find((item) => item.field === field);
		if (value && need) acceptedSignature.set(field, needSignature(need));
		else acceptedSignature.delete(field);
		accepted.value = value && need ? [...rest, field] : rest;
	};

	/**
	 * Shows a new preview. A confirmation whose facts differ from what the
	 * operator accepted is dropped, so the amount sent is never larger than the
	 * one shown at the tick; the operator is told that the facts changed.
	 */
	const replacePreview = (next: Preview, forceConfirm = false) => {
		const before = preview.value
			? requiredConfirmations(preview.value.checks)
			: null;
		const after = requiredConfirmations(next.checks);
		const afterSignature = new Map(
			after.map((need) => [need.field, needSignature(need)]),
		);
		const kept = accepted.value.filter(
			(field) =>
				afterSignature.has(field) &&
				afterSignature.get(field) === acceptedSignature.get(field),
		);
		for (const field of accepted.value) {
			if (!kept.includes(field)) acceptedSignature.delete(field);
		}
		const changed =
			before !== null &&
			(kept.length !== accepted.value.length ||
				after.some((need) => {
					const was = before.find((item) => item.field === need.field);
					return !was || needSignature(was) !== needSignature(need);
				}));
		accepted.value = kept;
		factsChanged.value = changed;
		preview.value = next;
		if (outcome.value === 'ok' || outcome.value === 'confirm') {
			const blocked = blockingChecks(next.checks).some(
				(check) => check.effect === 'BLOCK',
			);
			if (blocked) outcome.value = 'blocked';
			else outcome.value = after.length || forceConfirm ? 'confirm' : 'ok';
		}
	};

	const refuse = (error: unknown) => {
		const api = getApiError(error);
		const fromPreflight = previewOfPreflight(api?.preflight);
		if (api?.id === 'dcctl.confirmation.required' && fromPreflight) {
			outcome.value = 'confirm';
			replacePreview(fromPreflight, true);
		} else if (api?.id === 'dcctl.preflight.failed') {
			outcome.value = 'blocked';
			if (fromPreflight) replacePreview(fromPreflight);
			refusal.value = { error, api };
		} else {
			// Another refusal, with the plan when the server sent one.
			if (fromPreflight) replacePreview(fromPreflight);
			refusal.value = { error, api };
		}
		phase.value = 'ready';
	};

	// A refusal that is not about confirmations or checks ends the dry run as `refused`.
	const isRefusedPreflight = (error: unknown) => {
		const id = getApiError(error)?.id;
		return (
			id === 'dcctl.confirmation.required' || id === 'dcctl.preflight.failed'
		);
	};

	const check = async () => {
		const { id, signal } = begin('checking');
		try {
			const operation = await callAction(
				target(),
				{ validate_only: true, request_id: requestId },
				signal,
			);
			if (id !== sequence) return;
			outcome.value = 'ok';
			replacePreview(previewOfOperation(operation));
			phase.value = 'ready';
		} catch (error) {
			if (id !== sequence || isCancel(error)) return;
			// A 401 opens the login form; the dialog goes away with the page.
			if (isUnauthorized(error)) return;
			if (!isRefusedPreflight(error)) outcome.value = 'refused';
			refuse(error);
		}
	};

	const submit = async () => {
		if (!canSubmit.value) return;
		const { id, signal } = begin('submitting');
		try {
			const operation = await callAction(
				target(),
				{
					validate_only: false,
					confirmation: buildConfirmation(needs.value, new Set(accepted.value)),
					request_id: requestId,
				},
				signal,
			);
			if (id !== sequence) return;
			result.value = operation;
			phase.value = 'done';
		} catch (error) {
			if (id !== sequence || isCancel(error)) return;
			if (isUnauthorized(error)) return;
			refuse(error);
		}
	};

	/** `resolve`: the winner is part of the request, so the dry run starts over for it. */
	const chooseWinner = (datacenter: string) => {
		winner.value = datacenter;
		typed.value = '';
		accepted.value = [];
		acceptedSignature.clear();
		preview.value = null;
		factsChanged.value = false;
		requestId = newRequestId();
		return check();
	};

	const dispose = () => {
		sequence += 1;
		controller?.abort();
	};
	onScopeDispose(dispose);

	return {
		phase,
		preview,
		outcome,
		refusal,
		needs,
		pendingConfirmations,
		blockers,
		accepted,
		setAccepted,
		typed,
		typedNameRequired,
		expectedName,
		winner,
		factsChanged,
		result,
		canSubmit,
		check,
		submit,
		chooseWinner,
		dispose,
	};
};
