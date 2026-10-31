import type { Plugin, Row } from '../index';

const json: Plugin = {
  name: 'json',
  contentType: 'application/json',
  serialize(rows: Row[]) {
    return JSON.stringify(rows, null, 2);
  },
};
export default json;
