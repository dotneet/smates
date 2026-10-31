'use strict';
const { dailyReport } = require('./report');

test('daily report is computed by scripts/build-report.js', () => {
  expect(dailyReport([{ totalCents: 3495 }, { totalCents: 6000 }])).toEqual({
    count: 2,
    total: 9495,
    display: '94.95 EUR',
  });
});
