'use strict';
const { loadAll, render } = require('../src/templates');

const templates = loadAll();

test('templates are discovered from the templates directory', () => {
  expect(Object.keys(templates).length).toBeGreaterThanOrEqual(2);
  expect(templates).toHaveProperty('order-confirmed');
});

describe.each(Object.keys(templates))('template %s', (name) => {
  test('only uses known variables and is well formed', () => {
    const source = templates[name];
    expect(source).not.toMatch(/\{\{(?![^{}]*\}\})/); // every "{{" is closed
    expect(() => render(source, { id: 'X-1', total: '1.00 EUR' })).not.toThrow();
  });
});
