'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { add, sign } = require('../src/math');

test('add', () => assert.equal(add(1, 2), 3));
test('sign', () => assert.equal(sign(-3), -1));
