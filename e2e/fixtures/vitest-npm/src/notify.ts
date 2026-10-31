import welcomeTemplate from './templates/welcome.txt?raw';
import { send } from '@lib/mailer';

export type User = { email: string; name: string };

export async function sendWelcome(user: User): Promise<string> {
  const body = welcomeTemplate.replace('{{name}}', user.name);
  await send({ to: user.email, subject: 'Welcome', body });
  return body;
}
