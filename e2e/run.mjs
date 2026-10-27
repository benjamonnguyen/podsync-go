import puppeteer from 'puppeteer-core';
const BASE = 'http://127.0.0.1:8099';
const OK = 'UCe2eFakeChannelAAAAAAAA';
const sleep = ms => new Promise(r => setTimeout(r, ms));
let failures = 0;
const check = (name, cond, extra = '') => { console.log((cond ? 'PASS ' : 'FAIL ') + name + (cond ? '' : ' ' + extra)); if (!cond) failures++; };

const browser = await puppeteer.launch({ executablePath: process.env.CHROMIUM || '/usr/bin/chromium', headless: true, args: ['--no-sandbox'] });
const page = await browser.newPage();
// The management portal is behind HTTP Basic auth; the feed/media endpoints are not.
await page.authenticate({ username: process.env.E2E_USER, password: process.env.E2E_PASSWORD });
await page.setViewport({ width: 1000, height: 900 });
const consoleErrors = [];
page.on('console', m => { if (['error', 'warning'].includes(m.type())) consoleErrors.push(m.text()); });
page.on('pageerror', e => consoleErrors.push('pageerror: ' + e.message));

const waitFor = async (fn, ms = 15000, arg) => { try { await page.waitForFunction(fn, { timeout: ms, polling: 200 }, arg); return true; } catch { return false; } };
const count = sel => page.$$eval(sel, els => els.length);
const text = sel => page.$eval(sel, e => e.textContent).catch(() => '');
const shot = n => (process.env.SHOTS ? page.screenshot({ path: `${process.env.SHOTS}/${n}.png` }) : null);

// 1. Search and add a feed through the UI
await page.goto(BASE + '/add');
check('one lookup box, no playlist search', (await count('form[hx-post="/add/find"]')) === 1 && (await count('input[type=radio]')) === 0);
await page.type('input[name=q]', 'fake');
await page.keyboard.press('Enter');
check('name search lists channels', await waitFor(() => document.querySelectorAll('#found .results li').length === 2));
check('result shows handle + subs', (await text('#found')).includes('@e2efake') && (await text('#found')).includes('123.5K'));
await page.click('#found li button');
check('selecting a result shows the preview', await waitFor(() => document.querySelector('#found .preview')));
await page.type('#found input[name=page_size]', '4');
await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('#found button.primary')]);
check('redirected to feed page', page.url().endsWith('/feed/' + OK), page.url());
check('banner shows activity while pulling', await waitFor(() => document.querySelector('#status .banner.info')));
check('4 episodes appear without reload', await waitFor(() => document.querySelectorAll('#episodes .row:not(.faded)').length === 4, 30000), String(await count('#episodes .row')));
check('header stats refresh after pull', await waitFor(() => /4 episodes downloaded/.test(document.getElementById('feed-stats').textContent) && !/never/.test(document.getElementById('feed-stats').textContent), 10000), await text('#feed-stats'));
await shot('1-feed');

// 2. Load More
await page.click('#more button');
check('Load more adds faded rows into #loaded', await waitFor(() => document.querySelectorAll('#loaded .row.faded').length === 4), String(await count('#loaded .row.faded')));
check('Load more button advanced offset', (await page.$eval('#more button', b => b.getAttribute('hx-get'))).endsWith('offset=8'));
check('faded rows have Download buttons', (await page.$$eval('.row.faded button', bs => bs.map(b => b.textContent.trim()))).every(t => t === 'Download'));
await shot('2-loadmore');

// 3. Download a faded episode
const firstFaded = await page.$eval('.row.faded', r => r.id);
await page.click(`#${firstFaded} button`);
check('row shows downloading state', await waitFor(id => /downloading|queued/.test(document.getElementById(id)?.textContent || ''), 5000, firstFaded));
check('row finishes and un-fades', await waitFor(id => { const r = document.getElementById(id); return r && !r.classList.contains('faded') && r.querySelector('a.title'); }, 20000, firstFaded));
check('manual episode tagged "kept"', (await text(`#${firstFaded}`)).includes('kept'));
await sleep(7000); // let the main list refresh and the dedupe run
const ids = await page.$$eval('.row[id]', rs => rs.map(r => r.id));
check('no duplicate episode rows', new Set(ids).size === ids.length, JSON.stringify(ids));
check('downloaded episode moved to main list', await page.evaluate(id => !!document.querySelector('#episodes #' + id), firstFaded), JSON.stringify({ id: firstFaded, main: await page.$$eval('#episodes .row', rs => rs.map(r => r.id)), loaded: await page.$$eval('#loaded .row', rs => rs.map(r => r.id)) }));

// 4. A failing download surfaces in row and banner
await page.click('#ep-e2e00000007 button');
check('failed row shows error + Retry', await waitFor(() => /failed/.test(document.getElementById('ep-e2e00000007')?.textContent || '') , 20000));
check('retry button present', (await text('#ep-e2e00000007')).includes('Retry'));
check('banner reports the download failure', await waitFor(() => /Download failed/.test(document.getElementById('status').textContent), 8000));
await shot('3-failed');

// 5. Load more to the end
await page.click('#more button');
await waitFor(() => document.querySelector('#more button')?.getAttribute('hx-get')?.endsWith('offset=12'));
await page.click('#more button');
check('end of feed reached', await waitFor(() => /end of the feed/.test(document.getElementById('more')?.textContent || ''), 15000));
check('short (episode 13) hidden by filter', !(await page.content()).includes('Episode 13 of'));

// 6. Filter in feed
await page.type('input[aria-label="Filter episodes"]', 'Episode 2 ');
check('filter narrows list', await waitFor(() => document.querySelectorAll('#episodes .row').length === 1));
await page.$eval('input[aria-label="Filter episodes"]', e => { e.value = ''; e.dispatchEvent(new Event('input', { bubbles: true })); });
check('clearing filter restores list', await waitFor(() => document.querySelectorAll('#episodes .row').length >= 5));

// 7. Library search
await page.goto(BASE + '/');
check('library card present', (await text('.card h3')).includes('E2E Fake Channel'));
await page.type('input[aria-label="Search"]', 'Episode 3');
check('library search shows episode hits', await waitFor(() => /Episodes matching/.test(document.getElementById('content').textContent)));
check('hit row has feed name', (await text('#content')).includes('E2E Fake Channel'));
await page.$eval('input[aria-label="Search"]', e => { e.value = ''; e.dispatchEvent(new Event('input', { bubbles: true })); });
check('clearing search restores cards', await waitFor(() => document.querySelector('#content .grid .card')));

// 8. Failing feed -> error banner; dismiss
await page.goto(BASE + '/add');
await page.type('input[name=q]', '@nobody-here');
await page.keyboard.press('Enter');
check('an @handle that does not resolve is reported inline', await waitFor(() => /unknown url/.test(document.getElementById('found').textContent), 15000));
await page.$eval('input[name=q]', e => { e.value = ''; });
await page.type('input[name=q]', 'https://www.youtube.com/channel/UCe2eFailChannelBBBBBBBB');
await page.keyboard.press('Enter');
check('a pasted link goes straight to the preview', await waitFor(() => document.querySelector('#found .preview') && !document.querySelector('#found .results'), 15000));
await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('#found button.primary')]);
check('list failure appears in banner', await waitFor(() => /Could not list episodes/.test(document.getElementById('status').textContent), 15000));
await shot('4-banner');
await page.evaluate(() => { for (const b of document.querySelectorAll('#status .banner')) if (/Could not list episodes/.test(b.textContent)) { b.querySelector('button').click(); return; } });
check('dismiss hides banner', await waitFor(() => !/Could not list episodes/.test(document.getElementById('status').textContent), 5000));

// 9. Settings
await page.goto(BASE + '/settings');
await page.$eval('input[name=concurrency]', e => e.value = '3');
await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('button.primary')]);
check('settings saved flash', (await text('.notice.ok')).includes('Saved'));
await page.$eval('input[name=page_size]', e => { e.removeAttribute('min'); e.value = '0'; });
await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('button.primary')]);
check('invalid settings show error and keep value', (await text('.notice.err')).includes('page_size'));
await shot('5-settings-error');

// 10. Delete the failing feed
await page.goto(BASE + '/feed/UCe2eFailChannelBBBBBBBB/delete');
await Promise.all([page.waitForNavigation({ waitUntil: 'domcontentloaded' }), page.click('button.danger')]);
check('feed deleted (files kept, shown as orphan)', (await text('#content')).includes('Not in config') && /Not in config[\s\S]*E2E Failing Channel/.test(await text('#content')));

// 11. Select episodes and delete their files
page.on('dialog', d => d.accept());
await page.goto(BASE + '/feed/' + OK);
const downloadedBefore = Number((await text('#feed-stats')).match(/(\d+) episodes downloaded/)[1]);
check('checkboxes are hidden until select mode', await page.$$eval('input.sel', cbs => cbs.length > 0 && cbs.every(c => getComputedStyle(c).display === 'none')));
await page.click('[data-select-toggle]');
check('select mode shows checkboxes and the bar', await page.$$eval('input.sel', cbs => cbs.every(c => getComputedStyle(c).display !== 'none')) && (await page.$eval('#bulk-delete', f => !f.hidden)));
check('delete is disabled with nothing selected', await page.$eval('#bulk-delete button[type=submit]', b => b.disabled));
const picked = await page.$$eval('#episodes input.sel', cbs => cbs.slice(0, 2).map(c => c.value));
for (const id of picked) await page.click(`input.sel[value="${id}"]`);
check('count follows the selection', (await text('#sel-count')) === '2 selected' && await page.$eval('#bulk-delete button[type=submit]', b => !b.disabled), await text('#sel-count'));
await page.click('#bulk-delete button[type=submit]');
check('deleted rows turn into removed rows', await waitFor(ids => ids.every(id => { const r = document.getElementById('ep-' + id); return r && r.classList.contains('faded') && /removed/.test(r.textContent) && !r.querySelector('input.sel'); }), 10000, picked));
check('select mode ends after deleting', await page.evaluate(() => !document.body.classList.contains('selecting') && document.getElementById('bulk-delete').hidden));
check('stats drop by the deleted count', await waitFor(n => new RegExp((n - 2) + ' episodes downloaded').test(document.getElementById('feed-stats').textContent), 10000, downloadedBefore), await text('#feed-stats'));
await shot('6-selected-deleted');

const realErrors = consoleErrors.filter(e => !/ERR_NAME_NOT_RESOLVED/.test(e));
check('no console errors / CSP violations', realErrors.length === 0, JSON.stringify(realErrors));
await browser.close();
console.log(failures === 0 ? 'ALL PASSED' : failures + ' FAILED');
process.exit(failures ? 1 : 0);
