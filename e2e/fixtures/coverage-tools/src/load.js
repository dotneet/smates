'use strict';

const path = require('path');

// The module name is computed, so the dependency on src/plugin.js is only observable at
// run time.
exports.loadPlugin = () => require(path.join(__dirname, ['plug', 'in'].join('') + '.js'));
