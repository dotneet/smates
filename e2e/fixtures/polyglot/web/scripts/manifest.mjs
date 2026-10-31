#!/usr/bin/env node
// Build step: derives the PWA manifest from public/manifest.base.json and prints it.
import { readFileSync } from 'node:fs';

const base = JSON.parse(readFileSync(new URL('../public/manifest.base.json', import.meta.url), 'utf8'));
const manifest = { ...base, start_url: '/', display: 'standalone', icons: [{ src: '/icon-192.png', sizes: '192x192' }] };
process.stdout.write(JSON.stringify(manifest));
