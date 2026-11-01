'use strict';

exports.add = (a, b) => a + b;

exports.sign = (x) => {
  if (x > 0) return 1;
  if (x < 0) return -1;
  return 0;
};
