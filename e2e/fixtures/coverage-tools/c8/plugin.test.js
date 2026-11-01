'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { loadPlugin } = require('../src/load');

test('plugin', () => assert.equal(loadPlugin().answer(), 42));
