import { beforeEach, expect, test, vi } from 'vitest';

vi.mock('@lib/mailer', () => ({ send: vi.fn(async () => ({ id: 'mocked' })) }));

import { send } from '@lib/mailer';
import { sendWelcome } from '@lib/notify';

beforeEach(() => vi.mocked(send).mockClear());

test('sends the welcome template', async () => {
  const body = await sendWelcome({ email: 'a@example.com', name: 'Ada' });
  expect(body.split('\n')[0]).toBe('Welcome aboard, Ada!');
  expect(send).toHaveBeenCalledWith({ to: 'a@example.com', subject: 'Welcome', body });
});
