import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { test } from 'node:test';
import { chromium } from 'playwright-core';

const defaultChrome = '/usr/bin/google-chrome';

function dashboardEnvironment() {
  const dashboardUrl = process.env.DASHBOARD_URL;
  const managementKey = process.env.DASHBOARD_MANAGEMENT_KEY;
  const executablePath = process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || defaultChrome;
  assert.ok(dashboardUrl, 'DASHBOARD_URL must point to a disposable local CPA instance');
  assert.ok(managementKey, 'DASHBOARD_MANAGEMENT_KEY must be set for that instance');
  assert.equal(process.env.DASHBOARD_E2E_ALLOW_MUTATIONS, '1', 'set DASHBOARD_E2E_ALLOW_MUTATIONS=1 only for an isolated test instance');
  assert.ok(existsSync(executablePath), `Chromium executable does not exist: ${executablePath}`);
  assert.ok(['127.0.0.1', 'localhost', '::1'].includes(new URL(dashboardUrl).hostname), 'DASHBOARD_URL must use loopback');
  return { dashboardUrl, managementKey, executablePath };
}

// Fixture format: CPAMC v1.27.0 src/utils/encryption.ts and src/services/storage/secureStorage.ts.
function cpamcStorage(value, version, host, userAgent) {
  const key = Buffer.from(`cli-proxy-api-webui::secure-storage|${host}${version === 1 ? `|${userAgent}` : ''}`);
  const bytes = Buffer.from(JSON.stringify(value));
  return `enc::v${version}::${Buffer.from(bytes.map((byte, index) => byte ^ key[index % key.length])).toString('base64')}`;
}

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

async function recordDashboardUsage(page, baseUrl, managementKey, routerModel, providerModels) {
  const managementHeaders = { Authorization: 'Bearer ' + managementKey };
  const keys = await page.request.get(baseUrl + '/v0/management/api-keys', { headers: managementHeaders });
  assert.ok(keys.ok(), 'the disposable CPA should expose a client key');
  const [key] = (await keys.json())['api-keys'];
  assert.ok(key, 'the disposable CPA needs a client key');
  const headers = { Authorization: 'Bearer ' + key };
  let registered = false;
  for (let poll = 0; poll < 100 && !registered; poll++) {
    const models = await page.request.get(baseUrl + '/v1/models', { headers, timeout: 2000 });
    if (models.ok()) registered = (await models.json()).data.some((item) => item.id === routerModel);
    if (!registered) await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.ok(registered, 'the saved router alias should be registered in CPA');

  for (let index = 0; index < 4; index++) {
    const reply = await page.request.post(baseUrl + '/v1/chat/completions', {
      headers,
      data: { model: routerModel, messages: [{ role: 'user', content: index === 1 ? 'dashboard fixture fail east' : 'dashboard usage fixture' }] },
    });
    assert.equal(reply.status(), 200, 'the disposable CPA should execute each real routed request');
    assert.equal((await reply.json()).model, routerModel);
  }
  const params = new URLSearchParams({ router_model: routerModel, dimension: 'router_model' });
  for (let poll = 0; poll < 100; poll++) {
    const groups = await page.request.get(baseUrl + '/v0/management/plugins/model-router/usage/groups?' + params, { headers: managementHeaders });
    assert.ok(groups.ok());
    const [group] = (await groups.json()).items;
    if (group?.requests === 5) {
      assert.deepEqual(group.provider_subtotals.map((item) => item.provider_model).sort(), [...providerModels].sort());
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.fail('the four completions and failed east attempt should appear in router usage');
}

test('dashboard reuses CPAMC credentials and preserves manual recovery', { timeout: 120_000 }, async (t) => {
  const { dashboardUrl, managementKey, executablePath } = dashboardEnvironment();
  const { host, origin } = new URL(dashboardUrl);
  const fixtureUserAgent = 'CPAMC-session-fixture/1';
  const auth = (key) => ({ state: { apiBase: origin, managementKey: key, rememberPassword: Boolean(key) }, version: 0 });
  const stored = (value, version = 2) => cpamcStorage(value, version, host, fixtureUserAgent);
  const currentAuth = stored(auth(managementKey));
  const scenarios = [
    { name: 'v2 saved session', local: { 'cli-proxy-auth': currentAuth } },
    { name: 'v2 survives a changed user agent', local: { 'cli-proxy-auth': currentAuth }, userAgent: 'CPAMC-session-fixture/2' },
    { name: 'v1 saved session', local: { 'cli-proxy-auth': stored(auth(managementKey), 1) } },
    { name: 'v1 standalone key', session: { managementKey: stored(managementKey, 1) } },
    { name: 'legacy plaintext key', local: { managementKey } },
    { name: 'v2 overrides legacy entries', local: { 'cli-proxy-auth': currentAuth, managementKey: 'stale-fixture-key' }, session: { managementKey: 'stale-fixture-key', 'cli-proxy-auth': stored(auth('stale-fixture-key'), 1) } },
    { name: 'v2 session storage', session: { 'cli-proxy-auth': currentAuth } },
    { name: 'missing storage', fallback: true },
    { name: 'remembering disabled does not revive legacy keys', local: { 'cli-proxy-auth': stored(auth(undefined)), managementKey }, session: { managementKey }, fallback: true },
    { name: 'corrupt v2 does not revive legacy keys', local: { 'cli-proxy-auth': 'enc::v2::not-base64!', managementKey }, fallback: true },
    { name: 'corrupt v1 standalone key', session: { managementKey: 'enc::v1::not-base64!' }, fallback: true },
    { name: 'encoded non-JSON key', session: { managementKey: stored(managementKey).slice(0, -4) }, fallback: true },
    { name: 'unsupported saved session', local: { 'cli-proxy-auth': 'enc::v3::unsupported' }, fallback: true },
    { name: 'unsupported standalone key', session: { managementKey: 'enc::v3::unsupported' }, fallback: true },
    { name: 'rejected key is not retried after reload', local: { 'cli-proxy-auth': stored(auth('rejected-fixture-key')) }, fallback: true, rejected: true },
  ];
  const browser = await chromium.launch({ executablePath, headless: true, args: ['--no-sandbox', '--disable-setuid-sandbox'] });
  try {
    for (const scenario of scenarios) await t.test(scenario.name, async () => {
      const context = await browser.newContext({ userAgent: scenario.userAgent || fixtureUserAgent });
      try {
        const local = scenario.local || {};
        const session = scenario.session || {};
        await context.addInitScript(({ origin, local, session }) => {
          if (location.origin !== origin) return;
          for (const [key, value] of Object.entries(local)) localStorage.setItem(key, value);
          for (const [key, value] of Object.entries(session)) sessionStorage.setItem(key, value);
        }, { origin, local, session });
        const page = await context.newPage();
        const pageErrors = [];
        const managementRequests = [];
        page.on('pageerror', (error) => pageErrors.push(error.message));
        page.on('request', (request) => {
          if (new URL(request.url()).pathname.startsWith('/v0/management/')) managementRequests.push(request);
        });
        await page.goto(dashboardUrl, { waitUntil: 'domcontentloaded' });
        if (scenario.fallback) {
          await page.locator('#auth-dock').waitFor({ state: 'visible' });
          await page.waitForFunction(() => !document.getElementById('connect').disabled);
          assert.equal(await page.locator('#workspace').isVisible(), false);
          if (scenario.rejected) {
            assert.ok(managementRequests.length > 0);
            const requestCount = managementRequests.length;
            await page.reload({ waitUntil: 'domcontentloaded' });
            await page.locator('#auth-dock').waitFor({ state: 'visible' });
            assert.equal(managementRequests.length, requestCount, 'a rejected saved key must not be retried');
          } else assert.equal(managementRequests.length, 0, 'unavailable credentials must not reach the Management API');
          await page.locator('#management-key').fill(managementKey);
          await page.locator('#connect').click();
        }
        await page.locator('#workspace').waitFor({ state: 'visible' });
        await page.locator('#model-status[data-tone="ready"]').waitFor();
        assert.equal(await page.locator('#auth-dock').isVisible(), false);
        await page.getByRole('tab', { name: 'Usage tracking' }).click();
        await waitForUsageReady(page);
        if (!scenario.rejected) {
          assert.ok(managementRequests.length > 0);
          assert.ok(managementRequests.every((request) => request.headers().authorization === `Bearer ${managementKey}`));
        }
        const hostStorage = await page.evaluate(() => [localStorage, sessionStorage].map((storage) =>
          Object.fromEntries(['cli-proxy-auth', 'managementKey'].map((key) => [key, storage.getItem(key)]))));
        assert.deepEqual(hostStorage, [local, session].map((storage) =>
          Object.fromEntries(['cli-proxy-auth', 'managementKey'].map((key) => [key, storage[key] ?? null]))));
        assert.deepEqual(pageErrors, []);
      } finally {
        await context.close();
      }
    });
  } finally {
    await browser.close();
  }
});

test('dashboard tabs retain their browser interactions after source assembly', { timeout: 120_000 }, async () => {
  const { dashboardUrl, managementKey, executablePath } = dashboardEnvironment();

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

    const modelA = 'east/dashboard-e2e-model-a';
    const modelB = 'west/dashboard-e2e-model-b';
    for (let index = 0; index < 5; index++) {
      await page.locator('#add-route').click();
      let card = page.locator('.route-card').nth(index);
      assert.equal(await card.locator('details[open]').count(), 1, 'a new draft opens immediately');
      await card.locator('input[data-field="alias"]').fill(`dashboard-e2e-${index}`);
      if (index === 0) {
        await card.locator('select[data-field="strategy"]').selectOption('round-robin');
        card = page.locator('.route-card').nth(index);
      }
      await card.locator('select[data-target-field="model"]').selectOption(modelA);
      if (index === 0) await card.locator('input[data-target-field="weight"]').fill('3');
    }
    const modelOptions = await page.locator('.route-card').first().locator('select[data-target-field="model"] option').evaluateAll((options) => options.map((option) => option.value));
    assert.ok(modelOptions.includes(modelA));
    assert.ok(modelOptions.includes(modelB));

    let firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('button[data-action="add-target"]').click();
    firstRoute = page.locator('.route-card').first();
    await firstRoute.locator('.target-row').nth(1).locator('select[data-target-field="model"]').selectOption(modelB);
    await firstRoute.locator('.target-row').nth(1).locator('input[data-target-field="weight"]').fill('1');
    await firstRoute.locator('.target-row').nth(1).locator('button[data-action="target-up"]').click();
    firstRoute = page.locator('.route-card').first();
    assert.deepEqual(await firstRoute.locator('select[data-target-field="model"]').evaluateAll((selects) => selects.map((select) => select.value)), [
      modelB,
      modelA,
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

    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.locator('#workspace').waitFor({ state: 'visible' });
    await page.locator('#model-status[data-tone="ready"]').waitFor();
    const cards = page.locator('.route-card');
    assert.deepEqual(await routeAliases(page), beforeAliases);
    assert.equal(await cards.locator('details[open]').count(), 0);
    const firstSummary = cards.first().locator('summary');
    assert.match(await firstSummary.innerText(), /dashboard-e2e-0\s+2 targets/);
    await firstSummary.focus();
    await page.keyboard.press('Enter');
    const secondSummary = cards.nth(1).locator('summary');
    await secondSummary.focus();
    await page.keyboard.press('Space');
    assert.equal(await cards.locator('details[open]').count(), 2);
    assert.equal(await page.locator('#save-state').getAttribute('data-dirty'), 'false');
    await cards.nth(1).locator('[data-field="alias"]').fill('dashboard-e2e-edited');
    assert.match(await secondSummary.innerText(), /dashboard-e2e-edited/);
    await secondSummary.click();
    await secondSummary.click();
    assert.equal(await cards.nth(1).locator('[data-field="alias"]').inputValue(), 'dashboard-e2e-edited');
    await cards.nth(1).locator('[data-field="alias"]').fill(beforeAliases[1]);
    assert.equal(await page.locator('#save-state').getAttribute('data-dirty'), 'false');
    await secondSummary.click();
    assert.equal(await cards.locator('details[open]').count(), 1);

    await recordDashboardUsage(page, new URL(dashboardUrl).origin, managementKey, beforeAliases[0], [modelA, modelB]);
    await page.getByRole('tab', { name: 'Usage tracking' }).click();
    await page.locator('#usage-panel').waitFor({ state: 'visible' });
    await waitForUsageReady(page);
    assert.equal(await page.locator('#group-dimension').inputValue(), 'router_model');
    const routerTotal = page.locator('#usage-groups .router-total');
    await routerTotal.waitFor();
    assert.equal(await routerTotal.count(), 1);
    assert.match(await routerTotal.innerText(), /dashboard-e2e-0/);
    const providers = page.locator('.provider-subtotal .provider-subtotal-name');
    assert.deepEqual((await providers.allTextContents()).sort(), [modelA, modelB]);
    assert.match(await page.locator('#group-page-status').innerText(), /of 1 groups/);
    assert.equal(await page.locator('#usage-requests tr').count(), 5);
    await page.locator('#usage-requests').getByText(modelA, { exact: true }).first().waitFor();
    const retryRow = page.locator('#usage-requests tr').filter({ hasText: 'dashboard-e2e-0-retry' });
    await retryRow.getByText(modelB, { exact: true }).waitFor();
    for (const selector of [
      '#usage-range', '#usage-granularity', '#usage-router-model', '#usage-provider-model',
      '#usage-source', '#usage-service-tier', '#usage-result', '#usage-refresh',
      '#group-dimension', '#group-page-size', '#request-page-size', '#token-chart',
      '#model-chart', '#cost-chart', '#efficiency-chart',
    ]) assert.equal(await page.locator(selector).count(), 1, `${selector} should be present`);

    await page.locator('#usage-range').selectOption('custom');
    assert.equal(await page.locator('#usage-custom-range').isVisible(), true);
    const to = new Date(Date.now() + 60_000);
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
    const saveGrouping = page.waitForResponse((response) =>
      response.url().endsWith('/usage/preferences') && response.request().method() === 'PUT' && response.ok() &&
      response.request().postDataJSON().group_dimension === 'provider_model'
    );
    await page.locator('#group-dimension').selectOption('provider_model');
    await saveGrouping;
    await page.reload({ waitUntil: 'domcontentloaded' });
    await page.locator('#workspace').waitFor({ state: 'visible' });
    await page.getByRole('tab', { name: 'Usage tracking' }).click();
    await waitForUsageReady(page);
    assert.equal(await page.locator('#group-dimension').inputValue(), 'provider_model');

    assert.deepEqual(pageErrors, [], 'dashboard should not raise browser runtime errors');
    assert.deepEqual(failedApiResponses, [], 'dashboard management requests should succeed');
  } finally {
    await browser.close();
  }
});
