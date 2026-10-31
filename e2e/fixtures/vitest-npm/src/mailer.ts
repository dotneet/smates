import net from 'node:net';

export type Mail = { to: string; subject: string; body: string };

// Talks SMTP to the relay configured in SMTP_HOST; tests mock this module.
export async function send(mail: Mail): Promise<{ id: string }> {
  const host = process.env.SMTP_HOST;
  if (!host) throw new Error('SMTP_HOST is not configured');
  await new Promise<void>((resolve, reject) => {
    const sock = net.connect(25, host, () => {
      sock.end(`MAIL TO:<${mail.to}>\r\nSUBJECT:${mail.subject}\r\n\r\n${mail.body}\r\n.\r\n`);
      resolve();
    });
    sock.on('error', reject);
  });
  return { id: `${Date.now()}` };
}
