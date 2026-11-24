import { setupServer } from 'msw/node';

import { handlers } from './handlers';

/** Node msw server for vitest. */
export const server = setupServer(...handlers);
