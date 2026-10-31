import { words, capitalize } from '../text.js';

export default function title(s) {
  return words(s).map(capitalize).join(' ');
}
