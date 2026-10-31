'use strict';
// Entry point of the service (HTTP wiring omitted in this fixture).
const { dispatch } = require('./handlers');
const { Inventory } = require('./inventory');

module.exports = { dispatch, Inventory };
