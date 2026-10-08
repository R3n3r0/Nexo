import { setupServer } from 'msw/node';

import { defaultHandlers } from './handlers';

/** MSW server shared by the HTTP-layer tests; reset after every test. */
export const server = setupServer(...defaultHandlers);
