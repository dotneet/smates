import { slugify } from '@fx/core';

export function userHandle(displayName: string, id: number): string {
  return `${slugify(displayName)}-${id}`;
}
