// globalSetup: runs once in the main vitest process; data reaches tests through inject().
import { readFileSync } from 'node:fs';
import type { TestProject } from 'vitest/node';

export type Customer = { id: number; name: string; email: string; region: string };

declare module 'vitest' {
  export interface ProvidedContext {
    customers: Customer[];
  }
}

export default function setup(project: TestProject) {
  const customers: Customer[] = JSON.parse(readFileSync('test/seed/customers.json', 'utf8'));
  project.provide('customers', customers);
}
