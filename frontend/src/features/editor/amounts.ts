// An editor estimate only. The server recalculates and owns the confirmed amount.
export type Estimate = Readonly<{ subtotal: string; tax: string; total: string }>;

export class EstimateError extends Error {
  readonly code: 'INVALID_AMOUNT' | 'AMOUNT_LIMIT';
  readonly field: 'unit_price' | 'quantity' | 'total';
  constructor(code: 'INVALID_AMOUNT' | 'AMOUNT_LIMIT', field: 'unit_price' | 'quantity' | 'total') {
    super(code); this.name = 'EstimateError'; this.code = code; this.field = field;
  }
}

function fixed(value: string, decimals: number, field: 'unit_price' | 'quantity'): bigint {
  const grammar = new RegExp(`^[0-9]{1,9}(?:\\.[0-9]{1,${decimals}})?$`, 'u');
  // JavaScript '$' can match before a final newline: require the whole value.
  if (typeof value !== 'string' || value.length > 10 + decimals || grammar.exec(value)?.[0] !== value) {
    throw new EstimateError('INVALID_AMOUNT', field);
  }
  const [integer = '', fraction = ''] = value.split('.');
  return BigInt(integer + fraction.padEnd(decimals, '0'));
}

function amount(cents: bigint): string {
  const value = cents.toString().padStart(3, '0');
  return value.slice(0, -2) + '.' + value.slice(-2);
}

export function calculateEstimate(unitPrice: string, quantity: string): Estimate {
  const unit = fixed(unitPrice, 2, 'unit_price');
  const count = fixed(quantity, 6, 'quantity');
  const subtotal = (unit * count + 500000n) / 1000000n;
  const tax = (subtotal * 16n + 50n) / 100n;
  const total = subtotal + tax;
  if (total > 99999999999n) throw new EstimateError('AMOUNT_LIMIT', 'total');
  return Object.freeze({ subtotal: amount(subtotal), tax: amount(tax), total: amount(total) });
}
