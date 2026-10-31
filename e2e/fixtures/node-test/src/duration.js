import ms from 'ms';

export function humanize(millis) {
  return ms(millis, { long: true });
}

export function parseTimeout(s) {
  const v = ms(s);
  if (typeof v !== 'number') throw new Error(`bad duration: ${s}`);
  return v;
}
