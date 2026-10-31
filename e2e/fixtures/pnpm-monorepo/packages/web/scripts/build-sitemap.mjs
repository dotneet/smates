// Prints a JSON sitemap of the content/ directory: one entry per markdown page with its
// title (first heading).
import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const content = join(dirname(fileURLToPath(import.meta.url)), '..', 'content');
const pages = readdirSync(content)
  .filter((f) => f.endsWith('.md'))
  .sort()
  .map((f) => {
    const title = readFileSync(join(content, f), 'utf8').match(/^#\s+(.+)$/m)?.[1] ?? f;
    return { path: '/' + f.replace(/\.md$/, ''), title };
  });
process.stdout.write(JSON.stringify(pages));
