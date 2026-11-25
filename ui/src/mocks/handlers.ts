import { HttpResponse, http } from 'msw';
import {
	getGetDatacenterMockHandler,
	getListDatacentersMockHandler,
} from '@/api/gen/datacenter-service/datacenter-service.msw';
import { getListJournalEntriesMockHandler } from '@/api/gen/journal-service/journal-service.msw';
import {
	getGetOperationMockHandler,
	getListOperationsMockHandler,
} from '@/api/gen/operation-service/operation-service.msw';
import { getVerifyStatusMockHandler } from '@/api/gen/status-service/status-service.msw';
import { encodeBasicToken } from '@/utils/basicAuth';
import { example } from './example';
import { currentStatusResponse } from './statusVariants';

/** The only credentials the mock accepts. Mock-only, not a real account. */
export const MOCK_USER = {
	name: 'support',
	password: 'mock-password',
} as const;

const authorized = (request: Request) =>
	request.headers.get('Authorization') ===
	`Basic ${encodeBasicToken(MOCK_USER.name, MOCK_USER.password)}`;

/**
 * Like the real API: a missing or wrong `Authorization` gives `401`
 * `dcctl.auth.required` without `WWW-Authenticate`.
 */
const requireAuth = http.all('*/v1/*', ({ request }) => {
	if (authorized(request)) return undefined;
	return HttpResponse.json(example('/v1/status', 'get', '401'), {
		status: 401,
	});
});

export const handlers = [
	requireAuth,
	http.get('*/v1/status', () => currentStatusResponse()),
	getVerifyStatusMockHandler(example('/v1/status:verify', 'get')),
	getListDatacentersMockHandler(example('/v1/datacenters', 'get')),
	getGetDatacenterMockHandler(example('/v1/datacenters/{datacenter}', 'get')),
	getListOperationsMockHandler(example('/v1/operations', 'get')),
	getGetOperationMockHandler(example('/v1/operations/{operation}', 'get')),
	getListJournalEntriesMockHandler(example('/v1/journalEntries', 'get')),
];
