import { readFileSync } from 'node:fs';

// Every web test runs with a signed-in session loaded from a fixture file.
const session = JSON.parse(readFileSync(new URL('./fixtures/session.json', import.meta.url), 'utf8'));
(globalThis as any).__session = session;
