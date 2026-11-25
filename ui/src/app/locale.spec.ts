import { describe, expect, it } from 'vitest';

import {
	WebitelDcctlV1OperationMode,
	WebitelDcctlV1OperationState,
	WebitelDcctlV1OperationStepState,
	WebitelDcctlV1OperationStepType,
	WebitelDcctlV1OperationType,
} from '@/api/gen/_models';
import { detectLocale, messages } from './locale';

describe('detectLocale', () => {
	it.each([
		[['uk-UA', 'en-US'], 'uk'],
		[['en-GB'], 'en'],
		[['ru-RU', 'uk'], 'uk'],
		[['de-DE'], 'en'],
		[[], 'en'],
	] as const)('%j -> %s', (languages, expected) => {
		expect(detectLocale(languages)).toBe(expected);
	});
});

describe('operation labels', () => {
	const groups = {
		operationType: WebitelDcctlV1OperationType,
		stepType: WebitelDcctlV1OperationStepType,
		operationState: WebitelDcctlV1OperationState,
		operationMode: WebitelDcctlV1OperationMode,
		stepState: WebitelDcctlV1OperationStepState,
	};

	it.each(Object.entries(groups))(
		'%s has a label in en and uk for every value of the API enum',
		(group, values) => {
			for (const locale of ['en', 'uk'] as const) {
				const labels = (
					messages[locale].dcctl as unknown as Record<
						string,
						Record<string, string>
					>
				)[group];
				for (const value of Object.values(values)) {
					expect(labels?.[value], `${locale} ${group} ${value}`).toBeTruthy();
				}
			}
		},
	);
});
