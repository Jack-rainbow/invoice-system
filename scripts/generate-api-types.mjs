import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { resolve } from 'node:path';

// Deliberately small JSON Schema subset. Reject unsupported constructs rather than silently widen.
const supported = new Set(['$ref', 'type', 'enum', 'oneOf', 'anyOf', 'properties', 'required',
  'additionalProperties', 'items', 'description', 'title', 'format', 'pattern', 'minLength',
  'maxLength', 'minimum', 'maximum', 'minItems', 'maxItems', 'uniqueItems', 'writeOnly',
  'readOnly', 'default', 'examples', 'const']);

export function generateTypes(document) {
  const schemas = document.components?.schemas;
  if (!schemas || typeof schemas !== 'object') throw new Error('Missing components.schemas');
  const names = Object.keys(schemas).sort();
  for (const name of names) if (!/^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name)) throw new Error(`Unsupported schema name: ${name}`);
  function render(schema, location) {
    if (!schema || typeof schema !== 'object' || Array.isArray(schema)) throw new Error(`Invalid schema: ${location}`);
    for (const key of Object.keys(schema)) if (!supported.has(key) && !key.startsWith('x-')) throw new Error(`Unsupported schema keyword ${key}: ${location}`);
    if (schema.$ref) {
      const prefix = '#/components/schemas/';
      if (!schema.$ref.startsWith(prefix) || !Object.hasOwn(schemas, schema.$ref.slice(prefix.length))) throw new Error(`Unsupported reference: ${schema.$ref}`);
      if (Object.keys(schema).some((key) => !['$ref', 'description'].includes(key) && !key.startsWith('x-'))) throw new Error(`Unsupported reference sibling: ${location}`);
      return schema.$ref.slice(prefix.length);
    }
    if (schema.oneOf || schema.anyOf) {
      if (schema.oneOf && schema.anyOf || schema.type || schema.enum || schema.const !== undefined) throw new Error(`Unsupported combined schema: ${location}`);
      const alternatives = schema.oneOf ?? schema.anyOf;
      if (!Array.isArray(alternatives) || alternatives.length === 0) throw new Error(`Invalid union schema: ${location}`);
      return alternatives.map((item, i) => render(item, `${location}/${i}`)).join(' | ');
    }
    if (schema.const !== undefined) return literal(schema.const, location);
    if (schema.enum) {
      if (!Array.isArray(schema.enum) || schema.enum.length === 0) throw new Error(`Invalid enum schema: ${location}`);
      return schema.enum.map((value) => literal(value, location)).join(' | ');
    }
    if (Array.isArray(schema.type)) {
      if (schema.type.length === 0) throw new Error(`Invalid type union schema: ${location}`);
      return schema.type.map((type) => render({ ...schema, type }, location)).join(' | ');
    }
    switch (schema.type) {
      case 'string': return 'string';
      case 'integer': case 'number': return 'number';
      case 'boolean': return 'boolean';
      case 'null': return 'null';
      case 'array': return `Array<${render(schema.items, `${location}/items`)}>`;
      case 'object': {
        if (schema.additionalProperties !== false) throw new Error(`Object schema requires additionalProperties:false: ${location}`);
        const properties = schema.properties ?? {};
        const required = schema.required ?? [];
        if (!Array.isArray(required) || required.some((key) => !Object.hasOwn(properties, key))) throw new Error(`Invalid required schema fields: ${location}`);
        const keys = Object.keys(properties).sort();
        if (keys.length === 0) return 'Record<string, never>';
        return `{\n${keys.map((key) => `  ${JSON.stringify(key)}${required.includes(key) ? '' : '?'}: ${render(properties[key], `${location}/${key}`).replaceAll('\n', '\n  ')};`).join('\n')}\n}`;
      }
      default: throw new Error(`Unsupported schema type: ${location}`);
    }
  }
  return '// GENERATED from api/openapi.json. Do not edit.\n// Runtime validation and server authorization remain mandatory.\n\n' + names.map((name) => `export type ${name} = ${render(schemas[name], name)};\n`).join('\n');
}

function literal(value, location) {
  if (value === null || ['string', 'boolean'].includes(typeof value) || typeof value === 'number' && Number.isFinite(value)) return JSON.stringify(value);
  throw new Error(`Unsupported literal schema: ${location}`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const args = process.argv.slice(2);
  if (args.length > 1 || args.some((arg) => arg !== '--check')) throw new Error('Usage: node scripts/generate-api-types.mjs [--check]');
  const source = new URL('../api/openapi.json', import.meta.url);
  const target = new URL('../frontend/src/shared/api/generated.ts', import.meta.url);
  const output = generateTypes(JSON.parse(await readFile(source, 'utf8')));
  if (args.includes('--check')) {
    let current;
    try { current = await readFile(target, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (current !== output) {
      process.stderr.write('Generated API types are stale; run the generator without --check.\n');
      process.exitCode = 1;
    } else process.stdout.write('Generated API types match the contract.\n');
  } else {
    await writeFile(target, output);
    process.stdout.write(`Generated ${fileURLToPath(target)}\n`);
  }
}
