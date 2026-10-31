// Loads the demo shopper every test renders against.
import { readFileSync } from 'node:fs';

const user = JSON.parse(readFileSync(new URL('./fixtures/user.json', import.meta.url), 'utf8'));
(globalThis as any).__SHOPPER__ = user;
