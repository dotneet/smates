import { expect, test } from 'vitest';
import { loadPlugin } from '../src/load.js';

test('plugin', () => expect(loadPlugin().answer()).toBe(42));
