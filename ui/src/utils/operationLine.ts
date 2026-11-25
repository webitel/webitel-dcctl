import type {
	WebitelDcctlV1Operation as Operation,
	WebitelDcctlV1OperationStep as Step,
} from '@/api/gen/_models';

/** The header shows an operation that is running or waiting for a decision. */
export const isOperationShown = (
	operation: Pick<Operation, 'state'> | undefined,
): boolean =>
	operation?.state === 'RUNNING' || operation?.state === 'INTERRUPTED';

/** The running step; for an interrupted operation the failed one, else the next pending. */
export const currentStep = (
	operation: Pick<Operation, 'steps'>,
): Step | undefined => {
	const steps = operation.steps ?? [];
	return (
		steps.find((step) => step.state === 'RUNNING') ??
		steps.find((step) => step.state === 'FAILED') ??
		steps.find((step) => step.state === 'PENDING')
	);
};
