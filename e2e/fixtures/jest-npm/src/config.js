'use strict';
// Loaded once per module registry: reads the JSON config as a side effect of require().
const fs = require('fs');
const path = require('path');

const file = path.join(__dirname, '..', 'config', 'app.json');
const config = Object.freeze(JSON.parse(fs.readFileSync(file, 'utf8')));

if (typeof config.taxRate !== 'number') {
  throw new Error('config: taxRate must be a number');
}

module.exports = config;
