import test from 'node:test';
import assert from 'node:assert/strict';
import { calculateEstimate, EstimateError } from '../../src/features/editor/amounts.ts';

test('editor estimates match the fixed-point invoice and tax acceptance examples', () => {
  for (const [price, quantity, subtotal, tax, total] of [
    ['100', '5', '500.00', '80.00', '580.00'],
    ['86.21', '5', '431.05', '68.97', '500.02'],
    ['0.10', '3', '0.30', '0.05', '0.35'],
    ['0.05', '0.1', '0.01', '0.00', '0.01'],
    ['0.04', '0.1', '0.00', '0.00', '0.00'],
    ['0', '999999999.999999', '0.00', '0.00', '0.00'],
    ['862068965.51', '1', '862068965.51', '137931034.48', '999999999.99'],
  ]) {
    assert.deepEqual(calculateEstimate(price, quantity), { subtotal, tax, total });
  }
  assert.deepEqual(calculateEstimate('00100.0', '005.000000'), calculateEstimate('100.00', '5'));
});

test('invalid precision, ASCII grammar and range failures are attributed to a field', () => {
  for (const value of ['', '-1', '+1', '.1', '1.', '1e2', 'NaN', 'Infinity', '1,000', '$1', '１', '١', ' 1', '1\n', '1000000000', '0000000000']) {
    assert.throws(() => calculateEstimate(value, '1'), (error) => error instanceof EstimateError && error.field === 'unit_price' && error.code === 'INVALID_AMOUNT');
    assert.throws(() => calculateEstimate('1', value), (error) => error instanceof EstimateError && error.field === 'quantity' && error.code === 'INVALID_AMOUNT');
  }
  assert.throws(() => calculateEstimate('0.001', '1'), { field: 'unit_price' });
  assert.throws(() => calculateEstimate('1', '0.0000001'), { field: 'quantity' });
  assert.throws(() => calculateEstimate('862068965.52', '1'), { code: 'AMOUNT_LIMIT', field: 'total' });
  assert.throws(() => calculateEstimate('999999999.99', '999999999.999999'), { code: 'AMOUNT_LIMIT', field: 'total' });
});
