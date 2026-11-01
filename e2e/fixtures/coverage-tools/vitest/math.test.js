import { expect, test } from 'vitest';
import { add, sign } from '../src/math.js';

test('add', () => expect(add(1, 2)).toBe(3));
test('sign', () => expect(sign(-3)).toBe(-1));
