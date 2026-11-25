'use strict';

const fs = require('node:fs');
const { chromium } = require(process.env.MEDIA_REHEARSAL_PLAYWRIGHT || 'playwright');

async function main() {
  const origin = process.env.MEDIA_REHEARSAL_ORIGIN;
  const viewer = process.env.MEDIA_REHEARSAL_VIEWER;
  const output = process.env.MEDIA_REHEARSAL_OUTPUT;
  const title = 'Rehearsal Studio';
  if (!origin || !viewer || !output || !origin.startsWith('http://127.0.0.1:')) throw new Error('Set the local rehearsal settings.');
  const browser = await chromium.launch({
    headless: true,
    executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE || undefined,
    args: ['--autoplay-policy=no-user-gesture-required'],
  });
  try {
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    const requests = [];
    const errors = [];
    page.on('pageerror', (err) => errors.push(err.message));
    page.on('response', (response) => {
      const path = new URL(response.url()).pathname;
      const match = /^\/media\/(live|vod)\/[0-9a-f]+\/parts\/([0-9]+p)\/([0-9]+)\.ts$/.exec(path);
      if (match) requests.push({ at_ms: Date.now(), kind: match[1], track: match[2], sequence: Number(match[3]), status: response.status() });
    });
    await page.goto(origin, { waitUntil: 'domcontentloaded' });
    await page.locator('#token').fill(viewer);
    await page.getByRole('button', { name: '连接', exact: true }).click();
    await page.locator('#channels').getByRole('button', { name: title, exact: true }).click();
    await page.evaluate(() => {
      const player = document.querySelector('#player');
      window.rehearsalSignals = [];
      for (const kind of ['waiting', 'playing', 'seeking', 'seeked', 'ended', 'error']) {
        player.addEventListener(kind, () => window.rehearsalSignals.push({
          kind, at_ms: Date.now(), media_seconds: player.currentTime, height: player.videoHeight, seeking: player.seeking,
        }));
      }
    });
    await page.waitForFunction(() => !document.querySelector('#watch').disabled, null, { timeout: 45_000 });
    await page.locator('#watch').click();
    await page.waitForFunction(() => !document.querySelector('#quality-level').disabled, null, { timeout: 45_000 });
    const rendered = async (height) => {
      await page.waitForFunction((size) => {
        const element = document.querySelector('#player');
        return element.videoHeight === size && !element.paused && element.readyState >= 2 && element.currentTime > 0;
      }, height, { timeout: 30_000 });
      return page.evaluate(() => {
        const element = document.querySelector('#player');
        return { at_ms: Date.now(), height: element.videoHeight, media_seconds: element.currentTime };
      });
    };
    await page.locator('#quality-level').selectOption({ label: '720p' });
    const verifiedHigh = await rendered(720);
    await page.locator('#quality-level').selectOption({ label: '360p' });
    const verifiedLow = await rendered(360);
    await page.waitForFunction(() => document.querySelector('#active-source').textContent === '备路', null, { timeout: 70_000 });
    const takeover = await rendered(360);
    await page.screenshot({ path: output + '.live.png' });
    const liveSignals = await page.evaluate(() => window.rehearsalSignals.slice());
    await page.waitForFunction(() => document.querySelector('#state').textContent === '已归档', null, { timeout: 140_000 });
    await page.locator('#reload').click();
    await page.locator('#recordings').getByRole('button', { name: /Rehearsal Studio/ }).first().click();
    await page.waitForFunction(() => !document.querySelector('#watch').disabled, null, { timeout: 15_000 });
    await page.locator('#watch').click();
    await page.waitForFunction(() => Number.isFinite(document.querySelector('#player').duration) &&
      document.querySelector('#player').duration > 50, null, { timeout: 25_000 });
    const seeks = [];
    for (const time of [30, 52]) {
      await page.locator('#player').evaluate((player, target) => { player.currentTime = target; }, time);
      await page.waitForFunction((target) => {
        const player = document.querySelector('#player');
        return !player.seeking && player.currentTime >= target - 0.3 && player.currentTime <= target + 3 && player.readyState >= 2;
      }, time, { timeout: 25_000 });
      seeks.push(await page.locator('#player').evaluate((player) => player.currentTime));
    }
    await page.screenshot({ path: output + '.vod.png' });
    if (errors.length) throw new Error(errors.join('\n'));
    fs.writeFileSync(output, JSON.stringify({
      runtime: browser.version(), high: verifiedHigh, low: verifiedLow, takeover, live_signals: liveSignals,
      vod_seeks: seeks, successful_segments: requests.filter((item) => item.status === 200 || item.status === 206),
      failed_segments: requests.filter((item) => item.status >= 400),
    }, null, 2) + '\n');
  } finally {
    await browser.close();
  }
}

main().catch((error) => { process.stderr.write(String(error.stack || error) + '\n'); process.exitCode = 1; });
