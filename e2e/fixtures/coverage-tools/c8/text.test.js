'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { shout } = require('../src/text');

test('shout', () => assert.equal(shout('hi'), 'HI!'));
