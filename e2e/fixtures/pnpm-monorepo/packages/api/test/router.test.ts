import { expect, it } from 'vitest';
import { loadRoutes } from '../src/router.ts';

it('discovers routes from the routes directory', async () => {
  const routes = await loadRoutes();
  expect([...routes.keys()]).toEqual(['/health', '/products']);
});

it('serves health and products', async () => {
  const routes = await loadRoutes();
  expect(await routes.get('/health')!({})).toEqual({ ok: true, core: '1.2.0' });
  expect(await routes.get('/products')!({ currency: 'EUR' })).toEqual([
    { id: 'p1', name: 'Mug', price: '12.50 €' },
    { id: 'p2', name: 'Poster', price: '20.00 €' },
  ]);
});
