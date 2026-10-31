export type Session = { user: string; roles: string[] };

export function navItems(session: Session): string[] {
  const items = ['home', 'pricing'];
  if (session.roles.includes('editor')) items.push('admin');
  return items;
}
