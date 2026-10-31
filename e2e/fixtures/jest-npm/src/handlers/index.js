'use strict';
// Commands are dispatched to src/handlers/<name>.js, loaded on demand.
const path = require('path');

const NAME = /^[a-z][a-z-]*$/;

function dispatch(name, payload, deps = {}) {
  if (!NAME.test(name) || name === 'index') {
    throw new Error(`unknown command: ${name}`);
  }
  const handler = require(path.join(__dirname, name));
  return handler.handle(payload, deps);
}

module.exports = { dispatch };
