import { HttpResponse, http } from 'msw';
import { getListDatacentersMockHandler } from '@/api/gen/datacenter-service/datacenter-service.msw';
import { getListJournalEntriesMockHandler } from '@/api/gen/journal-service/journal-service.msw';
import { getVerifyStatusMockHandler } from '@/api/gen/status-service/status-service.msw';
import { encodeBasicToken } from '@/utils/basicAuth';
import { currentDatacenterResponse } from './datacenterVariants';
import { example } from './example';
import {
	currentOperationResponse,
	currentOperationsResponse,
	watchResponse,
} from './operationVariants';
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
	http.get('*/v1/datacenters/:dc', ({ params }) =>
		currentDatacenterResponse(String(params.dc)),
	),
	http.get('*/v1/operations', ({ request }) =>
		currentOperationsResponse(new URL(request.url)),
	),
	// Before the plain operation: its `:operation` segment would match `id:watch` too.
	http.get('*/v1/operations/:operation\\:watch', ({ params, request }) =>
		watchResponse(String(params.operation), request.signal),
	),
	http.get('*/v1/operations/:operation', ({ params }) =>
		currentOperationResponse(String(params.operation)),
	),
	getListJournalEntriesMockHandler(example('/v1/journalEntries', 'get')),
];
