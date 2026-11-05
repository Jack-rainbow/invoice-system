// Invoked inside the real authenticated admin browser flow. No mock login.
import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import { mkdir } from 'node:fs/promises';

export async function verifyEditor(page, screenshots, prefix = 'editor', options = {}) {
  assert.equal(await page.locator('.editor-card input, .editor-card textarea').count(), 9);
  assert.equal(await page.locator('.estimate input').count(), 0);
  // Every editable draft field is exercised through its visible label.
  const text = [
    ['收件人名称', '测试收件人'], ['商品编码', '53131620'], ['单位代码', 'DPC'],
    ['商品名称与说明', '测试商品'], ['开具时间', '2026-11-04T10:00:00-06:00'],
    ['单位说明 选填', '测试单位说明'], ['认证显示时间 选填', '2026-11-04T10:01:00-06:00'],
  ];
  for (const [label, value] of text) {
    const control = page.getByLabel(label, { exact: true });
    await control.fill(value);
    assert.equal(await control.inputValue(), value);
  }
  const price = page.getByLabel('单价 MXN', { exact: true });
  const quantity = page.getByLabel('数量', { exact: true });
  const outputs = page.locator('.estimate output');
  async function expectAmounts(unit, count, expected) {
    await price.fill(unit); await quantity.fill(count);
    assert.deepEqual(await outputs.allTextContents(), expected);
  }
  await expectAmounts('100', '5', ['500.00', '80.00', '580.00']);
  await expectAmounts('86.21', '5', ['431.05', '68.97', '500.02']);
  await expectAmounts('0.01', '0.5', ['0.01', '0.00', '0.01']);
  for (const [unit, count, expectedError] of [
    ['1.001', '5', '请填写有效的非负单价和数量'],
    ['not-a-number', '5', '请填写有效的非负单价和数量'],
    ['999999999', '999999999', '总计超过允许范围'],
  ]) {
    await expectAmounts(unit, count, ['—', '—', '—']);
    assert.match(await page.locator('.field-error').innerText(), new RegExp(expectedError));
    assert.doesNotMatch(await page.locator('.editor-card').innerText(), /NaN|Infinity/);
  }
  await expectAmounts('100', '5', ['500.00', '80.00', '580.00']);
  const payload = '<img src=x onerror="globalThis.__injected=true">';
  await page.getByLabel('商品名称与说明').fill(payload);
  assert.equal(await page.getByLabel('商品名称与说明').inputValue(), payload);
  assert.equal(await page.evaluate(() => globalThis.__injected === true), false);
  assert.equal(await page.locator('.editor-card img').count(), 0);
  // The same editor is mounted with and without the server preview service; the
  // button must stay disabled only when no watermarked-preview channel exists.
  if (options.preview === 'server') {
    assert.equal(await page.getByRole('button', { name: '查看水印预览' }).isDisabled(), false);
  } else {
    // The same editor is mounted with and without the server preview service; the
  // button must stay disabled only when no watermarked-preview channel exists.
  if (options.preview === 'server') {
    assert.equal(await page.getByRole('button', { name: '查看水印预览' }).isDisabled(), false);
  } else {
    assert.equal(await page.getByRole('button', { name: '预览服务尚未接入' }).isDisabled(), true);
  }
  }
  await page.getByLabel('商品名称与说明').fill('测试商品');
  await mkdir(screenshots, { recursive: true });
  await page.screenshot({ path: resolve(screenshots, prefix + '-desktop.png'), fullPage: true });

  await page.setViewportSize({ width: 375, height: 812 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.screenshot({ path: resolve(screenshots, prefix + '-mobile.png'), fullPage: true });
}
