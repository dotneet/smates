export type Messages = Record<string, string>;

// Locale bundles are code-split: the module path is computed at runtime.
export async function loadMessages(locale: string): Promise<Messages> {
  const mod = await import(`./locales/${locale}.ts`);
  return mod.default as Messages;
}

export function t(messages: Messages, key: string, vars: Record<string, string> = {}): string {
  return (messages[key] ?? key).replace(/\{(\w+)\}/g, (_, k) => vars[k] ?? `{${k}}`);
}
