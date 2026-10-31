'use strict';
// Email templates: every *.tmpl file in templates/ is a template named after the file.
const fs = require('fs');
const path = require('path');

const DIR = path.join(__dirname, '..', 'templates');

function loadAll() {
  const out = {};
  for (const file of fs.readdirSync(DIR).sort()) {
    if (!file.endsWith('.tmpl')) continue;
    out[path.basename(file, '.tmpl')] = fs.readFileSync(path.join(DIR, file), 'utf8');
  }
  return out;
}

function render(source, vars) {
  return source.replace(/\{\{\s*(\w+)\s*\}\}/g, (_, key) => {
    if (!(key in vars)) throw new Error(`missing template variable: ${key}`);
    return String(vars[key]);
  });
}

module.exports = { DIR, loadAll, render };
