import type {
	WebitelDcctlV1Check as Check,
	WebitelDcctlV1Confirmation as Confirmation,
	WebitelDcctlV1OperationMode as Mode,
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1Preflight as Preflight,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';

/** What a dry run shows: the checks, the plan and the write mode. */
export interface Preview {
	checks: Check[];
	steps: Step[];
	mode?: Mode;
}

export type ConfirmationField = keyof Confirmation;

/** One confirmation the operator must give, with the checks that ask for it. */
export interface ConfirmationNeed {
	field: ConfirmationField;
	checks: Check[];
	/** `max_discard_bytes`: the amount the dry run shows; this is the bound that is sent. */
	bytes?: string;
}

const KNOWN_FIELDS: readonly ConfirmationField[] = [
	'isolation',
	'not_max_lsn',
	'max_discard_bytes',
];

const isConfirmationField = (value: unknown): value is ConfirmationField =>
	KNOWN_FIELDS.includes(value as ConfirmationField);

/** Preview from a successful dry run (`200`, an operation without a name). */
export const previewOfOperation = (
	operation: Pick<Operation, 'checks' | 'steps' | 'mode'>,
): Preview => ({
	checks: [...(operation.checks ?? [])],
	steps: [...(operation.steps ?? [])],
	mode: operation.mode,
});

/** Preview from the `preflight` of a refusal. */
export const previewOfPreflight = (
	preflight: Preflight | undefined,
): Preview | null =>
	preflight
		? {
				checks: [...(preflight.checks ?? [])],
				steps: [...(preflight.steps ?? [])],
				mode: preflight.mode,
			}
		: null;

/** Checks by importance: failures first, then warnings, the rest (passed, skipped, informational) last. */
export const groupChecks = (checks: readonly Check[]) => ({
	fail: checks.filter((check) => check.result === 'FAIL'),
	warn: checks.filter((check) => check.result === 'WARN'),
	rest: checks.filter(
		(check) => check.result !== 'FAIL' && check.result !== 'WARN',
	),
});

/**
 * The amount of WAL a `max_discard_bytes` check would discard, in its facts:
 * `divergence_bytes` for a diverged datacenter, `lag_bytes` for a failover
 * candidate. The API has no field for it. `null` when the facts do not say,
 * and no bound covers an amount that is not known.
 */
export const discardBytes = (check: Check): string | null => {
	const facts = check.facts ?? {};
	const value = facts.divergence_bytes ?? facts.lag_bytes;
	if (typeof value === 'number' && Number.isFinite(value) && value >= 0) {
		return String(Math.trunc(value));
	}
	if (typeof value === 'string' && /^\d+$/.test(value)) return value;
	return null;
};

const isConfirmCheck = (check: Check): boolean =>
	check.effect === 'CONFIRM' &&
	check.result === 'FAIL' &&
	isConfirmationField(check.confirmation);

/**
 * Confirmations the operator can give: failed checks with `effect=confirm`,
 * grouped by the confirmation field. A `max_discard_bytes` check without a
 * known amount cannot be confirmed (the server blocks on it) and is left out.
 */
export const requiredConfirmations = (
	checks: readonly Check[],
): ConfirmationNeed[] => {
	const needs = new Map<ConfirmationField, ConfirmationNeed>();
	for (const check of checks.filter(isConfirmCheck)) {
		const field = check.confirmation as ConfirmationField;
		const bytes = field === 'max_discard_bytes' ? discardBytes(check) : null;
		if (field === 'max_discard_bytes' && bytes === null) continue;
		const need = needs.get(field) ?? { field, checks: [] };
		need.checks.push(check);
		if (bytes !== null) {
			// One bound covers every check of the field: the largest amount.
			need.bytes =
				need.bytes === undefined || BigInt(bytes) > BigInt(need.bytes)
					? bytes
					: need.bytes;
		}
		needs.set(field, need);
	}
	return [...needs.values()];
};

/**
 * What the operator accepts with a confirmation: the amount for a bound, the
 * detail and facts of the checks otherwise. A new signature means other facts.
 */
export const needSignature = (need: ConfirmationNeed): string =>
	JSON.stringify([
		need.field,
		need.bytes ?? null,
		need.checks.map((check) => [check.id, check.detail, check.facts ?? null]),
	]);

/** Failed checks that no confirmation accepts: they reject the action. */
export const blockingChecks = (checks: readonly Check[]): Check[] =>
	checks.filter(
		(check) =>
			check.result === 'FAIL' &&
			(check.effect === 'BLOCK' ||
				(check.effect === 'CONFIRM' &&
					(!isConfirmationField(check.confirmation) ||
						(check.confirmation === 'max_discard_bytes' &&
							discardBytes(check) === null)))),
	);

/** The `confirmation` of the request: the accepted fields, with the bound for the amount. */
export const buildConfirmation = (
	needs: readonly ConfirmationNeed[],
	accepted: ReadonlySet<ConfirmationField>,
): Confirmation | undefined => {
	const confirmation: Confirmation = {};
	for (const need of needs) {
		if (!accepted.has(need.field)) continue;
		if (need.field === 'isolation') confirmation.isolation = true;
		else if (need.field === 'not_max_lsn') confirmation.not_max_lsn = true;
		else if (need.bytes !== undefined) {
			confirmation.max_discard_bytes = need.bytes;
		}
	}
	return Object.keys(confirmation).length ? confirmation : undefined;
};
