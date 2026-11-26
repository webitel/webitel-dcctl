import type { WebitelDcctlV1Violation as Violation } from '@/api/gen/_models';

export interface FormattedViolation {
	type: string;
	subject: string;
	/** May span several lines: the filter parser reports the position under the expression. Render with `white-space: pre-wrap`. */
	text: string;
}

/**
 * `violations[]` of an error as lines for the screen. A violation without a
 * description says nothing and is dropped. Line breaks of a description are
 * kept: they carry the position of a parser error.
 */
export const formatViolations = (
	violations: readonly Violation[] | undefined,
): FormattedViolation[] =>
	(violations ?? []).flatMap((violation) => {
		const text = violation.description?.replace(/\s+$/, '');
		if (!text) return [];
		return [
			{
				type: violation.type ?? '',
				subject: violation.subject ?? '',
				text,
			},
		];
	});
