#!/usr/bin/env node
import { formatReport } from '../src/report-format.js';

const rows = [
  { sku: 'A-100', cents: 1250 },
  { sku: 'B-200', cents: 99 },
];
process.stdout.write(formatReport(rows) + '\n');
