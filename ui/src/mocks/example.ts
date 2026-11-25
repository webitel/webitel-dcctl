import swagger from '../../../api/dcctl.swagger.json';

type Examples = Record<string, { examples?: Record<string, unknown> }>;
type Operation = { responses: Examples };

/** Response example of an operation from `api/dcctl.swagger.json`. */
export const example = <T>(path: string, method: string, status = '200'): T => {
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
