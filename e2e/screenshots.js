// SPDX-License-Identifier: AGPL-3.0-or-later
// Takes the admin page's pictures for the README and docs/ADMIN.md, from a
// relay you run (scripts/screenshots.sh starts one in Docker with made-up
// load). Signs in with ADMIN_PASSWORD.
//
//   ADMIN_PASSWORD=... node screenshots.js http://localhost:18281 ../docs/images
import { chromium } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

const [base = 'http://localhost:18281', out = '../docs/images'] = process.argv.slice(2)
const password = process.env.ADMIN_PASSWORD
if (!password) throw new Error('set ADMIN_PASSWORD')
mkdirSync(out, { recursive: true })

const browser = await chromium.launch()
const shots = [
  { scheme: 'light', w: 1280, h: 860, pages: ['status', 'health', 'settings', 'connect'], suffix: '' },
  { scheme: 'dark', w: 1280, h: 860, pages: ['status', 'settings'], suffix: '-dark' },
  { scheme: 'dark', w: 390, h: 844, pages: ['status'], suffix: '-phone', scale: 2 },
]
for (const s of shots) {
  const ctx = await browser.newContext({ viewport: { width: s.w, height: s.h }, colorScheme: s.scheme, deviceScaleFactor: s.scale || 1 })
  const page = await ctx.newPage()
  await page.goto(base + '/login')
  await page.getByLabel('Admin password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.waitForURL(/\/status$/)
  for (const p of s.pages) {
    await page.goto(`${base}/${p}`)
    await page.waitForLoadState('networkidle')
    const file = join(out, `admin-${p}${s.suffix}.png`)
    await page.screenshot({ path: file, fullPage: s.suffix !== '-phone' })
    console.log(file)
  }
  await ctx.close()
}
await browser.close()
