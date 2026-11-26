import { vi } from 'vitest';

/**
 * Waits for what the SDK renders from lazily loaded components, such as
 * wt-table or a routed view. The first load in a test file takes seconds on a
 * busy runner, well over the default of vi.waitFor.
 */
export const waitForLazy = <T>(check: () => T | Promise<T>): Promise<T> =>
	vi.waitFor(check, { timeout: 10_000 });
