import { expect, test } from 'vitest';
import { shout } from '../src/text.js';

test('shout', () => expect(shout('hi')).toBe('HI!'));
