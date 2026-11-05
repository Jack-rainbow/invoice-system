// Invoked inside the real authenticated admin browser flow. No mock login.
import assert from 'node:assert/strict';
import { resolve } from 'node:path';
import { mkdir } from 'node:fs/promises';

export async function verifyEditor(page, screenshots, prefix = 'editor', options = {}) {
  assert.equal(await page.locator('.form-column input[name], .form-column textarea[name]').count(), 9);
  assert.equal(await page.locator('.estimate input').count(), 0);
  // Every editable draft field is exercised through its visible label.
  const text = [
    ['公司 / 客户名称', '测试收件人'], ['产品代码', '53131620'], ['单位', 'DPC'],
    ['商品描述', '测试商品'], ['开具时间', '2026-11-04T10:00:00-06:00'],
    ['单位描述 选填', '测试单位说明'], ['认证显示时间 选填', '2026-11-04T10:01:00-06:00'],
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
    assert.doesNotMatch(await page.locator('.form-column').innerText(), /NaN|Infinity/);
  }
  await expectAmounts('100', '5', ['500.00', '80.00', '580.00']);
  if (options.preview === 'server') {
    // Live preview: the debounced auto render lights the sheet without a click,
    // and focusing a field draws its highlight rectangle on the page.
    try {
      await page.getByTestId('preview-image').waitFor({ timeout: 60000 });
    } catch {
      const errors = await page.locator('.field-error').allTextContents().catch(() => []);
      const notices = await page.locator('[data-testid="draft-notice"]').allTextContents().catch(() => []);
      console.error(`LIVE-PREVIEW-MISSING errors=${JSON.stringify(errors)} notices=${JSON.stringify(notices)}`);
      throw new Error('live preview did not render for the administrator editor');
    }
    await page.getByLabel('公司 / 客户名称', { exact: true }).focus();
    await page.getByTestId('field-highlight').waitFor({ timeout: 5000 });
    await page.getByLabel('公司 / 客户名称', { exact: true }).blur();
  }
  const payload = '<img src=x onerror="globalThis.__injected=true">';
  await page.getByLabel('商品描述').fill(payload);
  assert.equal(await page.getByLabel('商品描述').inputValue(), payload);
  assert.equal(await page.evaluate(() => globalThis.__injected === true), false);
  assert.equal(await page.locator('.form-column img').count(), 0);
  // The replicated toolbar gates formal exports on the confirmed live preview;
  // after the last edit the debounced re-render re-enables them.
  if (options.preview === 'server') {
    await page.waitForFunction(() => {
      const pdf = [...document.querySelectorAll('button')].find(b => b.textContent?.includes('导出 PDF'));
      return pdf instanceof HTMLButtonElement && !pdf.disabled;
    }, undefined, { timeout: 60000 });
    assert.equal(await page.getByRole('button', { name: '导出 PDF' }).isDisabled(), false);
    assert.equal(await page.getByRole('button', { name: '导出 PNG' }).isDisabled(), false);
  }
  await page.getByLabel('商品描述').fill('测试商品');
  await mkdir(screenshots, { recursive: true });
  await page.screenshot({ path: resolve(screenshots, prefix + '-desktop.png'), fullPage: true });

  await page.setViewportSize({ width: 375, height: 812 });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
  await page.screenshot({ path: resolve(screenshots, prefix + '-mobile.png'), fullPage: true });
}
