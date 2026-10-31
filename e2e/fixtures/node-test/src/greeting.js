import { t } from './i18n.js';

export function greet(name, lang = 'en') {
  return t(lang, 'greeting', { name });
}
