#!/usr/bin/env node
// Renders web/public/brand/social-preview.html to the 1280x640 PNG that GitHub
// serves as the repo's og:image. Re-run after any edit to the template.
//   NODE_PATH=<puppeteer-core dir> node scripts/social-preview.js
const path = require('path');
const fs = require('fs');
const puppeteer = require('puppeteer-core');

const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const SRC = path.resolve(__dirname, '../web/public/brand/social-preview.html');
const OUT = path.resolve(__dirname, '../web/public/brand/social-preview.png');

(async () => {
  const browser = await puppeteer.launch({ executablePath: CHROME, args: ['--no-sandbox'] });
  const page = await browser.newPage();
  // deviceScaleFactor 1: GitHub wants exactly 1280x640, not a retina multiple
  await page.setViewport({ width: 1280, height: 640, deviceScaleFactor: 1 });
  await page.goto('file://' + SRC, { waitUntil: 'networkidle0' });
  await page.evaluateHandle('document.fonts.ready'); // vendored Inter, never a system fallback
  const inter = await page.evaluate(() => document.fonts.check('800 112px Inter'));
  if (!inter) throw new Error('the vendored Inter did not load; the card would ship a system fallback');
  await page.screenshot({ path: OUT, type: 'png' });
  await browser.close();
  const size = fs.statSync(OUT).size;
  if (size > 1024 * 1024) throw new Error('card is over GitHub\'s 1 MB limit: ' + size);
  console.log('SOCIAL_PREVIEW_OK ' + OUT + ' ' + size + ' bytes');
})().catch((e) => { console.error('SOCIAL_PREVIEW_FAIL: ' + e.message); process.exit(1); });
