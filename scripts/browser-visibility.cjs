'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const { chromium } = require(process.env.MEDIA_REHEARSAL_PLAYWRIGHT || 'playwright');

async function main() {
  const origin = process.env.MEDIA_REHEARSAL_ORIGIN;
  const editor = process.env.MEDIA_REHEARSAL_EDITOR;
  const output = process.env.MEDIA_REHEARSAL_OUTPUT;
  if (!origin || !/^http:\/\/127\.0\.0\.1:\d+$/.test(origin) || !editor || !output) {
    throw new Error('Set the local rehearsal settings.');
  }
  const browser = await chromium.launch({
    headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
  });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    page.setDefaultTimeout(9000);
    const errors = [];
    page.on('pageerror', (err) => errors.push(err.message));
    const login = async () => {
      await page.locator('#token').fill(editor);
      await page.getByRole('button', { name: '连接', exact: true }).click();
      await page.locator('#visibility-batches').waitFor({ state: 'visible' });
    };
    const recorded = { prepares: 0, executions: 0, recovery_queries: 0 };
    let key;
    await page.route('**/api/visibility-batches/**', async (route) => {
      const req = route.request();
      const path = new URL(req.url()).pathname;
      if (req.method() === 'PUT' && !key) {
        key = path.split('/').at(-1);
        const response = await route.fetch();
        assert.equal(response.status(), 200);
        recorded.prepares++;
        return route.abort('connectionreset');
      }
      if (path === '/api/visibility-batches/' + key + '/execute' && req.method() === 'POST') {
        const response = await route.fetch();
        assert.equal(response.status(), 200);
        recorded.executions++;
        return route.abort('connectionreset');
      }
      if (path === '/api/visibility-batches/' + key && req.method() === 'GET') recorded.recovery_queries++;
      return route.continue();
    });
    await page.goto(origin, { waitUntil: 'domcontentloaded' });
    await login();
    const choice = page.getByRole('checkbox', { name: /Rehearsal Studio/ });
    await choice.check();
    const recording = await choice.getAttribute('data-batch-recording');
    await page.getByRole('button', { name: '保存选集', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#visibility-batch-receipts').textContent.includes('未开始'));
    assert.equal(recorded.prepares, 1);
    assert.equal(recorded.executions, 0);

    await page.reload({ waitUntil: 'domcontentloaded' });
    await login();
    await page.locator('#visibility-batch-history').selectOption(key);
    const readStatus = () => apiReceipt(page, origin, editor, key);
    const pending = await readStatus();
    assert.equal(pending.state, 'prepared');
    assert.equal(pending.items[0].recording_id, recording);
    await page.getByRole('button', { name: '执行待处理项目', exact: true }).click();
    await page.waitForFunction(() => document.querySelector('#visibility-batch-receipts').textContent.includes('已完成'));
    const finished = await readStatus();
    assert.equal(finished.state, 'completed');
    assert.equal(finished.processed, 1);
    assert.equal(finished.items[0].applied_revision, pending.items[0].expected_revision + 1);
    await page.getByRole('button', { name: '刷新批次', exact: true }).click();
    await page.locator('#visibility-batch-execute').waitFor({ state: 'hidden' });
    assert.equal(recorded.executions, 1);
    assert.ok(recorded.recovery_queries >= 3);
    assert.deepEqual(errors, []);
    fs.writeFileSync(output, JSON.stringify({
      runtime: browser.version(), request_key: key, recording_id: recording, held_across_reload: true,
      ...recorded, item: finished.items[0],
    }, null, 2) + '\n');
  } finally {
    await browser.close();
  }
}

async function apiReceipt(page, origin, token, key) {
  const response = await page.request.get(origin + '/api/visibility-batches/' + key, {
    headers: { Authorization: 'Bearer ' + token },
  });
  assert.equal(response.status(), 200);
  return response.json();
}

main().catch((error) => { process.stderr.write(String(error.stack || error) + '\n'); process.exitCode = 1; });
