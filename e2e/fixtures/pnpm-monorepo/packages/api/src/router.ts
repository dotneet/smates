import { readdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

export type Handler = (query: Record<string, string>) => unknown | Promise<unknown>;

const routesDir = join(dirname(fileURLToPath(import.meta.url)), 'routes');

// Routes are discovered from the routes/ directory: every module there registers the
// handler it exports as default under its file name. Adding a file adds a route.
export async function loadRoutes(): Promise<Map<string, Handler>> {
  const routes = new Map<string, Handler>();
  for (const f of readdirSync(routesDir).sort()) {
    if (!f.endsWith('.ts')) continue;
    const mod = await import(/* @vite-ignore */ join(routesDir, f));
    routes.set('/' + f.replace(/\.ts$/, ''), mod.default);
  }
  return routes;
}
