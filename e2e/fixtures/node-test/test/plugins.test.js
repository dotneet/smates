import { test } from 'node:test';
import assert from 'node:assert/strict';
import { listPlugins, loadPlugin, applyAll } from '../src/plugins/index.js';

test('discovers plugins from the directory', () => {
  assert.deepEqual(listPlugins(), ['exclaim', 'title']);
});

test('loads a plugin by computed name', async () => {
  const name = ['ti', 'tle'].join('');
  const title = await loadPlugin(name);
  assert.equal(title('hello big world'), 'Hello Big World');
});

test('applies all plugins in order', async () => {
  assert.equal(await applyAll('hello world'), 'Hello World!');
});
