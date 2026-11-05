import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const contract = JSON.parse(await readFile(new URL('../api/openapi.json', import.meta.url), 'utf8'));
const design = await readFile(new URL('../docs/specs/design.md', import.meta.url), 'utf8');
const operations = Object.entries(contract.paths).flatMap(([path, item]) => Object.entries(item)
  .filter(([method]) => ['get', 'head', 'post', 'put', 'patch', 'delete', 'options'].includes(method))
  .map(([method, operation]) => ({ path, method, operation })));

test('all design business operations are represented once with explicit runtime scope', () => {
  const section = design.split('## 8 ')[1].split('## 9 ')[0];
  const expected = new Set();
  for (const line of section.split('\n')) {
    const match = /^\| ((?:(?:GET|POST|PUT|PATCH|DELETE|HEAD) )+)(\/\S+) \|/.exec(line);
    if (match) for (const method of match[1].trim().split(' ')) expected.add(`${method} /api/v1${match[2]}`);
  }
  assert.equal(expected.size, 53, 'design operation extraction must cover all 53 business operations');
  const implemented = new Set(['GET /api/v1/auth/csrf', 'POST /api/v1/auth/admin/bootstrap/start', 'POST /api/v1/auth/admin/bootstrap/confirm', 'POST /api/v1/auth/mfa/verify', 'POST /api/v1/auth/mfa/step-up', 'GET /api/v1/admin/users', 'POST /api/v1/admin/users', 'POST /api/v1/admin/users/{id}/credit-grants', 'GET /api/v1/me', 'POST /api/v1/auth/device/verify', 'GET /api/v1/auth/device/context', 'POST /api/v1/documents', 'GET /api/v1/documents', 'GET /api/v1/documents/{id}', 'PATCH /api/v1/documents/{id}', 'DELETE /api/v1/documents/{id}', 'POST /api/v1/documents/{id}/previews', 'GET /api/v1/previews/{id}', 'GET /api/v1/previews/{id}/image', 'GET /api/v1/render-profiles', 'POST /api/v1/documents/{id}/render-profile', 'POST /api/v1/documents/{id}/generations', 'GET /api/v1/generations/{id}', 'GET /api/v1/documents/{id}/versions', 'GET /api/v1/versions/{id}/download', 'GET /api/v1/presets', 'POST /api/v1/presets', 'PATCH /api/v1/presets/{id}', 'DELETE /api/v1/presets/{id}', 'GET /api/v1/credits/entries', 'POST /api/v1/operations/lookup', 'POST /api/v1/auth/device/pairings', 'GET /api/v1/auth/device/transfers/{id}', 'PATCH /api/v1/admin/users/{id}/status', 'POST /api/v1/admin/users/{id}/password-reset', 'GET /api/v1/admin/users/{id}/device-binding', 'POST /api/v1/admin/users/{id}/device-reset', 'POST /api/v1/admin/users/{id}/device-transfers/{transfer_id}/cancel', 'GET /api/v1/admin/credit-entries', 'GET /api/v1/admin/audit-logs']);
  const partial = new Set(['POST /api/v1/auth/login', 'POST /api/v1/auth/mfa/challenges', 'POST /api/v1/auth/logout', 'POST /api/v1/auth/device/challenges', 'POST /api/v1/auth/password/set', 'POST /api/v1/auth/device/bind']);
  const actual = new Set(operations.filter(({ path }) => path.startsWith('/api/v1/')).map(({ path, method, operation }) => {
    const key = `${method.toUpperCase()} ${path}`;
    assert.equal(operation['x-implementation-status'], implemented.has(key) ? 'implemented' : partial.has(key) ? 'partial' : 'planned');
    if (implemented.has(key) || partial.has(key)) assert.ok(operation['x-runtime-scope']);
    return `${method.toUpperCase()} ${path}`;
  }));
  assert.deepEqual(actual, expected);
  assert.equal(new Set(operations.map(({ operation }) => operation.operationId)).size, operations.length);
});

test('bootstrap recovery codes have a one-time sensitive response and runtime errors are typed', () => {
  const schema = contract.components.schemas.BootstrapConfirmResponse;
  assert.ok(schema.required.includes('recovery_codes'));
  assert.equal(schema.properties.recovery_codes['x-sensitive'], true);
  assert.equal(contract.paths['/api/v1/auth/admin/bootstrap/confirm'].post.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/BootstrapConfirmResponse');
  assert.ok(contract.components.schemas.Error.properties.code.enum.includes('SETUP_LINK_ALREADY_ISSUED'));
});

test('all references resolve and object DTOs are closed', () => {
  function walk(value) {
    if (!value || typeof value !== 'object') return;
    if (value.$ref) {
      assert.ok(value.$ref.startsWith('#/'));
      let referenced = contract;
      for (const piece of value.$ref.slice(2).split('/')) referenced = referenced?.[piece.replace(/~1/g, '/').replace(/~0/g, '~')];
      assert.ok(referenced, `unresolved reference ${value.$ref}`);
    }
    if (value.type === 'object') assert.equal(value.additionalProperties, false);
    for (const nested of Object.values(value)) walk(nested);
  }
  walk(contract);
});

test('business security, write CSRF and mandatory financial idempotency cannot disappear', () => {
  const idempotent = new Set([
    'POST /api/v1/admin/users', 'POST /api/v1/admin/users/{id}/credit-grants',
    'POST /api/v1/admin/users/{id}/device-reset', 'POST /api/v1/documents/{id}/generations',
    'POST /api/v1/batch-drafts', 'POST /api/v1/batches',
  ]);
  for (const { method, path, operation } of operations) {
    if (!path.startsWith('/api/v1/')) continue;
    assert.equal(operation['x-csrf-required'], !['get', 'head', 'options'].includes(method));
    assert.equal(operation['x-idempotency-required'], idempotent.has(`${method.toUpperCase()} ${path}`));
    if (['B', 'M', 'MF'].includes(operation['x-auth-level'])) {
      assert.ok(operation.security.length > 0);
      assert.ok(operation.security.every((alternative) => Object.keys(alternative).length > 0));
    }
  }
  assert.deepEqual(contract.paths['/api/v1/auth/password/change'].post.security, [
    { UserSessionCookie: [], DeviceSignature: [] }, { RecentMFASessionCookie: [] },
  ]);
});

test('INITIAL_SETUP challenge has a token-authorized branch without a pre-existing session Cookie', () => {
  const operation = contract.paths['/api/v1/auth/device/challenges'].post;
  assert.ok(operation.security.some((alternative) => Object.keys(alternative).length === 0),
    'OpenAPI must not require a Cookie before initial token activation');
  const initial = contract.components.schemas.InitialChallengeRequest;
  assert.ok(initial.required.includes('setup_token'));
  assert.equal(initial.additionalProperties, false);
});

test('challenge and refresh context supply the binding epoch required by the device signer', () => {
  const { ChallengeResponse, DeviceContextResponse } = contract.components.schemas;
  assert.ok(ChallengeResponse.required.includes('binding_epoch'));
  assert.ok(ChallengeResponse.required.includes('purpose'));
  assert.ok(DeviceContextResponse.required.includes('binding_epoch'));
  assert.ok(ChallengeResponse.required.includes('key_thumbprint'));
  assert.ok(DeviceContextResponse.required.includes('key_thumbprint'));
  assert.deepEqual(ChallengeResponse.properties.purpose.enum,
    ['INITIAL_SETUP', 'PAIR', 'BIND', 'LOGIN', 'REQUEST']);
});

test('only completed and deliverable generation responses contain download paths', () => {
  const schemas = contract.components.schemas;
  const alternatives = schemas.GenerationResponse.oneOf.map(({ $ref }) => schemas[$ref.split('/').at(-1)]);
  for (const variant of alternatives) {
    if (variant.properties.state.enum.includes('COMPLETED')) {
      assert.deepEqual(variant.properties.billing_state.enum, ['COMPLETED']);
      assert.deepEqual(variant.properties.delivery_state.enum, ['READY']);
      assert.ok(variant.required.includes('downloads'));
    } else assert.equal(variant.properties.downloads, undefined);
  }
});
