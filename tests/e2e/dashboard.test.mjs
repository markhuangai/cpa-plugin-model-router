import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { test } from 'node:test';
import { chromium } from 'playwright-core';

const defaultChrome = '/usr/bin/google-chrome';

function localDateTime(date) {
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

async function waitForLayout(page) {
  await page.evaluate(() => new Promise((resolve) => {
    requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(resolve, 100)));
  }));
}

async function routeAliases(page) {
  return page.locator('.route-card input[data-field="alias"]').evaluateAll((inputs) => inputs.map((input) => input.value));
}

async function waitForUsageReady(page) {
  await page.waitForFunction(() => {
    const panel = document.querySelector('#usage-panel');
    const status = document.querySelector('#usage-status')?.textContent || '';
    return panel?.getAttribute('aria-busy') !== 'true' && !/^(Loading|Refreshing|Open Usage)/.test(status);
  }, { timeout: 20_000 });
  const status = await page.locator('#usage-status').textContent();
  assert.doesNotMatch(status || '', /Refresh failed|could not be loaded|could not be saved/i, status || '');
}

test('dashboard tabs retain their browser interactions after source assembly', { timeout: 120_000 }, async () => {
  const dashboardUrl = process.env.DASHBOARD_URL;
  const managementKey = process.env.DASHBOARD_MANAGEMENT_KEY;
  const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || defaultChrome;

  assert.ok(dashboardUrl, 'DASHBOARD_URL must point to a disposable local CPA instance');
  assert.ok(managementKey, 'DASHBOARD_MANAGEMENT_KEY must be set for that instance');
  assert.equal(process.env.DASHBOARD_E2E_ALLOW_MUTATIONS, '1', 'set DASHBOARD_E2E_ALLOW_MUTATIONS=1 only for an isolated test instance');
  assert.ok(existsSync(executablePath), `Chromium executable does not exist: ${executablePath}`);

  const parsedUrl = new URL(dashboardUrl);
  assert.ok(['127.0.0.1', 'localhost', '::1'].includes(parsedUrl.hostname), 'DASHBOARD_URL must use loopback');

  const browser = await chromium.launch({
    executablePath,
    headless: true,
    args: ['--no-sandbox', '--disable-setuid-sandbox'],
  });
  const page = await browser.newPage({ viewport: { width: 390, height: 844 } });
  const pageErrors = [];
  const failedApiResponses = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  page.on('response', (response) => {
    if (response.url().includes('/v0/management/') && response.status() >= 400) {
      failedApiResponses.push(`${response.status()} ${new URL(response.url()).pathname}`);
    }
  });

  try {
    await page.goto(dashboardUrl, { waitUntil: 'domcontentloaded' });
    await page.locator('#management-key').fill(managementKey);
    await page.locator('#connect').click();
    await page.locator('#workspace').waitFor({ state: 'visible' });
    await page.locator('#model-status[data-tone="ready"]').waitFor();

    assert.equal(await page.locator('[role="tab"]').count(), 2);
    assert.equal(await page.locator('#configuration-tab').getAttribute('aria-selected'), 'true');
    assert.deepEqual(await routeAliases(page), [], 'the disposable E2E instance should start without routes');

    for (let index = 0; index < 5; index++) {
      await page.locator('#add-route').click();
      let card = page.locator('.route-card').nth(index);
      await card.locator('input[data-field="alias"]').fill(`dashboard-e2e-${index}`);
      if (index === 0) {
        await card.locator('select[data-field="strategy"]').selectOption('round-robin');
        card = page.locator('.route-card').nth(index);
      }
      await card.locator('select[data-target-field="model"]').selectOption('dashboard-e2e-model-a');
      if (index === 0) await card.locator('input[data-target-field="weight"]').fill('3');
    }
    const modelOptions = await page.locator('.route-card').first().locator('select[data-target-field="model"] option').evaluateAll((options) => options.map((option) => option.value));
    assert.ok(modelOptions.includes('dashboard-e2e-model-a'));
    assert.ok(modelOptions.includes('dashboard-e2e-model-b'));

    let firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('button[data-action="add-target"]').click();
    firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('.target-row').nth(1).locator('select[data-target-field="model"]').selectOption('dashboard-e2e-model-b');
    await firstRoute.locator('.target-row').nth(1).locator('input[data-target-field="weight"]').fill('1');
    await firstRoute.locator('.target-row').nth(1).locator('button[data-action="target-up"]').click();
    firstRoute = page.locator('.route-card').first();
    assert.deepEqual(await firstRoute.locator('select[data-target-field="model"]').evaluateAll((selects) => selects.map((select) => select.value)), [
      'dashboard-e2e-model-b',
      'dashboard-e2e-model-a',
    ]);
    await firstRoute.locator('.target-row').first().locator('button[data-action="target-down"]').click();

    const beforeAliases = Array.from({ length: 5 }, (_, index) => `dashboard-e2e-${index}`);
    const moveUp = page.locator('.route-card[data-index="3"] button[data-action="up"]');
    await moveUp.scrollIntoViewIfNeeded();
    await waitForLayout(page);
    const scrollBefore = await page.evaluate(() => window.scrollY);
    await moveUp.click();
    await waitForLayout(page);
    const scrollAfter = await page.evaluate(() => window.scrollY);
    assert.ok(Math.abs(scrollBefore - scrollAfter) <= 1, `route reorder changed scrollY from ${scrollBefore} to ${scrollAfter}`);
    assert.deepEqual(await routeAliases(page), [beforeAliases[0], beforeAliases[1], beforeAliases[3], beforeAliases[2], beforeAliases[4]]);
    assert.equal(await page.evaluate(() => document.activeElement?.dataset.action), 'up');
    await page.keyboard.press('Enter');
    await waitForLayout(page);
    assert.deepEqual(await routeAliases(page), [beforeAliases[0], beforeAliases[3], beforeAliases[1], beforeAliases[2], beforeAliases[4]]);
    await page.locator('.route-card[data-index="1"] button[data-action="down"]').click();
    await page.locator('.route-card[data-index="2"] button[data-action="down"]').click();
    assert.deepEqual(await routeAliases(page), beforeAliases);

    await page.locator('#add-route').click();
    await page.locator('.route-card').last().locator('button[data-action="remove"]').click();
    assert.deepEqual(await routeAliases(page), beforeAliases);
    await page.locator('#save').click();
    await page.waitForFunction(() => document.querySelector('#save-state')?.textContent === 'Saved in CPA', { timeout: 20_000 });

    await page.getByRole('tab', { name: 'Usage tracking' }).click();
    await page.locator('#usage-panel').waitFor({ state: 'visible' });
    await waitForUsageReady(page);
    for (const selector of [
      '#usage-range', '#usage-granularity', '#usage-router-model', '#usage-provider-model',
      '#usage-source', '#usage-service-tier', '#usage-result', '#usage-refresh',
      '#group-dimension', '#group-page-size', '#request-page-size', '#token-chart',
      '#model-chart', '#cost-chart', '#efficiency-chart',
    ]) assert.equal(await page.locator(selector).count(), 1, `${selector} should be present`);

    await page.locator('#usage-range').selectOption('custom');
    assert.equal(await page.locator('#usage-custom-range').isVisible(), true);
    const to = new Date();
    const from = new Date(to.getTime() - 60 * 60 * 1000);
    await page.locator('#usage-from').fill(localDateTime(from));
    await page.locator('#usage-from').dispatchEvent('change');
    await page.locator('#usage-to').fill(localDateTime(to));
    await page.locator('#usage-to').dispatchEvent('change');
    await page.locator('#usage-granularity').selectOption('day');
    await page.locator('#group-dimension').selectOption('provider');
    await page.locator('#group-page-size').selectOption('25');
    await page.locator('#request-page-size').selectOption('25');
    await page.locator('#usage-result').selectOption('success');
    await page.locator('#usage-refresh').click();
    await waitForUsageReady(page);

    const sortButton = page.locator('#group-headers button[data-sort-field]').first();
    await sortButton.click();
    await waitForUsageReady(page);
    await page.locator('#group-columns summary').click();
    const column = page.locator('#group-column-options input[data-column-kind="group"]').first();
    const columnKey = await column.getAttribute('data-column-key');
    await column.uncheck();
    assert.equal(await page.locator(`#group-table th[data-column="${columnKey}"]`).count(), 0);
    await page.locator(`#group-column-options input[data-column-kind="group"][data-column-key="${columnKey}"]`).check();

    const tokenLegend = page.locator('[data-token-series="input"]');
    await tokenLegend.click();
    assert.equal(await tokenLegend.getAttribute('aria-pressed'), 'false');
    await tokenLegend.click();
    assert.equal(await tokenLegend.getAttribute('aria-pressed'), 'true');
    await page.getByRole('button', { name: 'Zoom token trend in' }).click();
    await page.getByRole('button', { name: 'Reset token trend zoom' }).click();

    await page.locator('#usage-pricing').click();
    await page.locator('#pricing-dialog[open]').waitFor();
    await page.locator('#pricing-model-name').fill('dashboard-e2e-price-model');
    await page.locator('#pricing-add').click();
    const priceRow = page.locator('.price-row').first();
    assert.equal(await priceRow.locator('[data-price-field="name"]').inputValue(), 'dashboard-e2e-price-model');
    await priceRow.locator('[data-price-field="input"]').fill('1.25');
    await priceRow.locator('[data-price-field="output"]').fill('2.5');
    await priceRow.locator('[data-price-field="cache_read"]').fill('0.1');
    await priceRow.locator('[data-price-field="cache_creation"]').fill('0.2');
    await priceRow.locator('[data-price-field="accounting_mode"]').selectOption('input_excludes_cache');
    await page.locator('#pricing-save').click();
    await page.locator('#pricing-dialog[open]').waitFor({ state: 'detached' });
    await page.locator('#usage-pricing').click();
    await page.locator('.price-row').waitFor();
    const savedPrice = page.locator('.price-row').first();
    assert.equal(await savedPrice.locator('[data-price-field="name"]').inputValue(), 'dashboard-e2e-price-model');
    assert.equal(await savedPrice.locator('[data-price-field="input"]').inputValue(), '1.25');
    assert.equal(await savedPrice.locator('[data-price-field="accounting_mode"]').inputValue(), 'input_excludes_cache');
    await page.locator('#pricing-cancel').click();

    await page.locator('#usage-reset').click();
    await page.locator('#reset-dialog[open]').waitFor();
    await page.locator('#reset-confirmation').fill('reset');
    assert.equal(await page.locator('#reset-confirm').isEnabled(), true);
    await page.locator('#reset-confirm').click();
    await page.locator('#reset-dialog[open]').waitFor({ state: 'detached' });
    await waitForUsageReady(page);

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.getByRole('tab', { name: 'Configuration' }).click();
    assert.equal(await page.locator('#configuration-panel').isVisible(), true);
    await page.getByRole('tab', { name: 'Usage tracking' }).click();
    assert.equal(await page.locator('#usage-panel').isVisible(), true);

    assert.deepEqual(pageErrors, [], 'dashboard should not raise browser runtime errors');
    assert.deepEqual(failedApiResponses, [], 'dashboard management requests should succeed');
  } finally {
    await browser.close();
  }
});
