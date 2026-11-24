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
import {
	getGetStatusMockHandler,
	getVerifyStatusMockHandler,
} from '@/api/gen/status-service/status-service.msw';
import { encodeBasicToken } from '@/utils/basicAuth';
import swagger from '../../../api/dcctl.swagger.json';

/** The only credentials the mock accepts. Mock-only, not a real account. */
export const MOCK_USER = {
	name: 'support',
	password: 'mock-password',
} as const;

type Examples = Record<string, { examples?: Record<string, unknown> }>;
type Operation = { responses: Examples };

/** Response example of an operation from `api/dcctl.swagger.json`. */
const example = <T>(path: string, method: string, status = '200'): T => {
	const paths = swagger.paths as unknown as Record<
		string,
		Record<string, Operation>
	>;
	const found =
		paths[path]?.[method]?.responses[status]?.examples?.['application/json'];
	if (found === undefined) {
		throw new Error(`no example for ${method} ${path} ${status}`);
	}
	return found as T;
};

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
	getGetStatusMockHandler(example('/v1/status', 'get')),
	getVerifyStatusMockHandler(example('/v1/status:verify', 'get')),
	getListDatacentersMockHandler(example('/v1/datacenters', 'get')),
	getGetDatacenterMockHandler(example('/v1/datacenters/{datacenter}', 'get')),
	getListOperationsMockHandler(example('/v1/operations', 'get')),
	getGetOperationMockHandler(example('/v1/operations/{operation}', 'get')),
	getListJournalEntriesMockHandler(example('/v1/journalEntries', 'get')),
];
