export type Row = Record<string, string | number>;
export type Plugin = { name: string; contentType: string; serialize(rows: Row[]): string };

// Output plugins live in ./builtin and are loaded on demand by name.
export async function loadPlugin(name: string): Promise<Plugin> {
  if (!/^[a-z]+$/.test(name)) throw new Error(`bad plugin name ${name}`);
  const mod = await import(`./builtin/${name}.ts`);
  return mod.default as Plugin;
}
