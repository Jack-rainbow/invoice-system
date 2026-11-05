import test from 'node:test';
import assert from 'node:assert/strict';
import { generateTypes } from './generate-api-types.mjs';

const spec = (schemas) => ({ openapi: '3.1.0', components: { schemas } });

test('required, optional, refs, enum, null and nested arrays are represented', () => {
  const output = generateTypes(spec({
    Item: { type: 'object', additionalProperties: false, required: ['id', 'state', 'tags'], properties: {
      id: { $ref: '#/components/schemas/ID' }, state: { enum: ['NEW', 'DONE'] },
      tags: { type: 'array', items: { type: 'array', items: { type: ['string', 'null'] } } },
      note: { type: ['string', 'null'] },
    } }, ID: { type: 'string', format: 'uuid' },
  }));
  assert.match(output, /export type ID = string;/);
  assert.match(output, /"id": ID;/);
  assert.match(output, /"state": "NEW" \| "DONE";/);
  assert.match(output, /"tags": Array<Array<string \| null>>;/);
  assert.match(output, /"note"\?: string \| null;/);
  assert.doesNotMatch(output, /\bany\b/);
});

test('oneOf, anyOf and literal primitive enum values', () => {
  const output = generateTypes(spec({ Choice: { oneOf: [{ type: 'null' }, { enum: [true, 1, 'a'] }] },
    Alternative: { anyOf: [{ type: 'string' }, { type: 'integer' }] } }));
  assert.match(output, /export type Choice = null \| true \| 1 \| "a";/);
  assert.match(output, /export type Alternative = string \| number;/);
});

test('deterministic output independent of object property insertion order', () => {
  const first = spec({ Z: { type: 'object', additionalProperties: false, properties: { b: { type: 'boolean' }, a: { type: 'integer' } } }, A: { type: 'string' } });
  const second = spec({ A: { type: 'string' }, Z: { type: 'object', additionalProperties: false, properties: { a: { type: 'integer' }, b: { type: 'boolean' } } } });
  assert.equal(generateTypes(first), generateTypes(second));
  assert.match(generateTypes(first), /GENERATED/);
});

test('unsupported, unsafe or malformed schemas fail explicitly', () => {
  const invalid = [ {}, { type: 'mystery' }, { type: 'array' }, { type: 'object' },
    { type: 'object', additionalProperties: true }, { $ref: 'https://example.com/remote' },
    { $ref: '#/components/schemas/Missing' }, { allOf: [{ type: 'string' }] },
    { type: 'string', nullable: true }, { type: 'object', additionalProperties: false, required: ['missing'], properties: {} },
    { type: 'string', surprise: true },
  ];
  for (const schema of invalid) assert.throws(() => generateTypes(spec({ Broken: schema })), /schema|supported|reference|required|nullable/i);
});
