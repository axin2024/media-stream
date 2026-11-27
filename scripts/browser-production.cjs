'use strict';

const assert = require('node:assert/strict');
const {chromium} = require(process.env.MEDIA_STREAM_TEST_PLAYWRIGHT || 'playwright');
const root = process.env.STUDIO_TEST_URL;
const token = process.env.STUDIO_TEST_TOKEN;
const production = process.env.STUDIO_TEST_PRODUCTION;
const digest = process.env.STUDIO_TEST_DIGEST;
const [from, to] = (process.env.STUDIO_TEST_RANGE || '').split(',').map(Number);

function timecode(frame) {
  const seconds = Math.floor(frame / 25);
  return [Math.floor(seconds / 3600), Math.floor(seconds / 60) % 60, seconds % 60, frame % 25]
    .map(value => String(value).padStart(2, '0')).join(':');
}

async function run() {
  assert(root && token && production && digest && to > from);
  const browser = await chromium.launch({
    executablePath: process.env.MEDIA_STREAM_TEST_CHROME || undefined,
    headless: true,
    args: ['--autoplay-policy=no-user-gesture-required'],
  });
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 960}});
    const pageErrors = [];
    page.on('pageerror', error => pageErrors.push(error.message));
    const mediaResponses = [];
    page.on('response', response => {
      if (response.url().includes('/media/')) mediaResponses.push([new URL(response.url()).pathname, response.status()]);
    });
    await page.goto(root);
    await page.getByLabel('访问凭据').fill(token);
    await page.getByRole('button', {name: '连接', exact: true}).click();
    await page.getByRole('button', {name: '制作导播', exact: true}).click();
    await page.locator('#production-select').selectOption(production);
    await page.locator('#production-state').getByText('已封存', {exact: true}).waitFor();
    assert.match(await page.locator('#production-program').textContent(), /CAM-B/);
    assert.match(await page.locator('#production-controller').textContent(), /relief-director.*代际 2.*CONT/);
    await page.waitForFunction(() => document.querySelectorAll('#production-monitors video').length === 4);
    assert.match(await page.locator('[data-slot="CAM-A"] p').textContent(), /-200 ms/);
    await page.getByRole('button', {name: '载入预监', exact: true}).click();
    await page.waitForFunction(() => Array.from(document.querySelectorAll('#production-monitors video'))
      .every(video => video.readyState >= 2 && video.videoWidth === 480 && video.currentTime > 0));
    const first = page.locator('[data-slot="CAM-A"]');
    await first.getByRole('button', {name: '试听'}).click();
    assert.deepEqual(await page.locator('#production-monitors video').evaluateAll(videos => videos.map(video => video.muted)), [false, true, true, true]);
    await first.getByRole('button', {name: '试听'}).click();
    await page.getByRole('button', {name: '播放主输出'}).click();
    await page.waitForFunction(() => document.querySelector('#production-video').readyState >= 2);

    await page.getByRole('button', {name: '读取实际播出单'}).click();
    const fallback = page.locator('#production-asrun-list li[data-reason="source_unavailable"]');
    await fallback.first().waitFor();
    const boundaries = await fallback.evaluateAll(nodes => nodes.map(node => [Number(node.dataset.start), Number(node.dataset.end)]));
    assert.equal(boundaries[0][0], from);
    assert.equal(boundaries.at(-1)[1], to);
    assert.equal(boundaries.reduce((total, [start, end]) => total + end-start, 0), to-from);
    const archive = page.locator('#production-archive-form');
    await archive.getByLabel('起始时码').fill(timecode(from));
    await archive.getByLabel('结束时码').fill(timecode(to));
    await archive.getByRole('button', {name: '核验所选区间'}).click();
    await page.waitForFunction(expected => document.querySelector('#production-archive-status').dataset.digest === expected, digest);
    assert.match(await page.locator('#production-archive-status').textContent(), /完整/);
    await page.getByRole('button', {name: '播放所选区间'}).click();
    await page.waitForFunction(() => {
      const video = document.querySelector('#production-archive-video');
      return video.readyState >= 2 && video.currentTime > 0 && Math.abs(video.duration - 10) <= 0.15;
    });
    assert(mediaResponses.some(([path, status]) => path.startsWith('/media/archive/') && status === 200));
    assert(mediaResponses.some(([path, status]) => path.includes('/parts/720p/') && status === 200));
    const downloadPromise = page.waitForEvent('download');
    await page.getByRole('button', {name: '导出交付清单'}).click();
    const download = await downloadPromise;
    assert.equal(download.suggestedFilename(), 'archive-' + production + '.json');
    await page.setViewportSize({width: 390, height: 844});
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    assert.deepEqual(pageErrors, []);
    console.log(JSON.stringify({browser: browser.version(), monitors: 4, alignment_ms: -200, programme_playable: true,
      verified_slate_intervals: boundaries, archive_digest: digest, archive_playable: true, download: true, viewport_width: 390, page_errors: 0}));
  } finally {
    await browser.close();
  }
}

run().catch(error => {
  console.error(error.message);
  process.exitCode = 1;
});
