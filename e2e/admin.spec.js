// SPDX-License-Identifier: AGPL-3.0-or-later
// WittyRelay's admin page in a real browser: first run, every page, settings,
// sign out and in, the keyboard, phones, dark mode, and no CSP violations.
import { test, expect } from '@playwright/test'

test.describe.configure({ mode: 'serial' })

const password = 'an e2e test password'
const pages = ['/status', '/health', '/settings', '/connect']

let page
const problems = []

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage()
  // The wrong token, password and value below answer 401 and 422 on purpose; anything else counts.
  const expected = /Failed to load resource: the server responded with a status of (401|422)/
  page.on('console', (m) => { if ((m.type() === 'error' || m.type() === 'warning') && !expected.test(m.text())) problems.push(m.text()) })
  page.on('pageerror', (e) => problems.push(String(e)))
})

test.afterAll(async () => {
  await page.close()
})

// A signed-out page (first run, signing in) is one card, centred both ways, with the relay's mark and name at its
// top, and fits without scrolling on any screen 720 pixels tall or more (WittyMediaServer's docs/DESIGN.md,
// "Standalone pages"). Checked at the sizes people use, then put back.
const SIZES = [[1280, 720], [1366, 768], [1440, 900], [1920, 1080], [360, 640], [390, 844]]
async function standalone(what) {
  for (const [width, height] of SIZES) {
    await page.setViewportSize({ width, height })
    const m = await page.evaluate(() => {
      const card = document.querySelector('[data-standalone-card]')
      const r = card.getBoundingClientRect()
      const mark = card.querySelector('.sa-brand svg').getBoundingClientRect()
      const root = document.scrollingElement
      return { left: r.left, right: innerWidth - r.right, top: r.top, bottom: innerHeight - r.bottom, scroll: root.scrollHeight - innerHeight, wide: root.scrollWidth - innerWidth,
        name: card.querySelector('.sa-brand').innerText.trim(), mark: mark.width > 8 && mark.height > 8 }
    })
    const at = `${what} at ${width}x${height}`
    expect(m.wide, `${at}: no sideways scrolling`).toBeLessThanOrEqual(1)
    expect(Math.abs(m.left - m.right), `${at}: centred across`).toBeLessThanOrEqual(4)
    expect(m.mark, `${at}: the relay's mark`).toBe(true)
    expect(m.name.length, `${at}: its name`).toBeGreaterThan(0)
    if (height >= 720) {
      expect(m.scroll, `${at}: no scrolling`).toBeLessThanOrEqual(1)
      expect(Math.abs(m.top - m.bottom), `${at}: centred down`).toBeLessThanOrEqual(4)
    }
  }
  await page.setViewportSize({ width: 1280, height: 720 })
}

test('first run: the setup token sets the password', async () => {
  await page.goto('/')
  await expect(page).toHaveURL(/\/setup$/)
  await expect(page.getByRole('heading', { name: 'Set the admin password' })).toBeVisible()
  await standalone('first run')

  await page.getByLabel('Setup token').fill('not-the-token')
  await page.getByLabel('New password').fill(password)
  await page.getByLabel('The same password again').fill(password)
  await page.getByRole('button', { name: 'Set password and sign in' }).click()
  await expect(page.getByRole('alert')).toContainText("isn't the setup token")

  await page.getByLabel('Setup token').fill('demo-setup-token')
  await page.getByLabel('New password').fill(password)
  await page.getByLabel('The same password again').fill(password)
  await page.getByRole('button', { name: 'Set password and sign in' }).click()
  await expect(page).toHaveURL(/\/connect\?welcome=1$/)
  await expect(page.getByRole('status')).toContainText('password is set')
  await expect(page.getByRole('heading', { name: 'Add the relay on the server' })).toBeVisible()
  await expect(page.locator('#compose')).toContainText('WITTYRELAY_ADMIN')
})

test('status shows counts and keeps itself current', async () => {
  await page.goto('/status')
  await expect(page.getByRole('heading', { level: 1, name: 'Status' })).toBeVisible()
  const nav = page.getByRole('navigation', { name: 'Admin pages' })
  await expect(nav.getByRole('link', { name: 'Status' })).toHaveAttribute('aria-current', 'page')
  await expect(page.locator('[data-k="servers"]').first()).toHaveText('5')
  await expect(page.getByRole('cell', { name: 'srv-1c9e4a' })).toBeVisible()
  await expect(page.getByText('Registration without a right access key')).toBeVisible()
  const relayed = page.locator('[data-k="relayed"]')
  const before = await relayed.textContent()
  await expect(relayed).not.toHaveText(before, { timeout: 12_000 })
})

test('health lists checks, addresses, ports and recent warnings', async () => {
  await page.goto('/health')
  await expect(page.getByText('Access keys required')).toBeVisible()
  await expect(page.locator('.addrs code').first()).toContainText('/p2p/')
  await expect(page.getByRole('cell', { name: 'admin sign-in: wrong password' })).toBeVisible()
  await expect(page.locator('body')).not.toContainText('demo-access-key-never-shown')
})

test('settings: a bad value is refused, a good one saved', async () => {
  await page.goto('/settings')
  const kbps = page.getByLabel('Speed per connection')
  await kbps.fill('50')
  await page.getByRole('button', { name: 'Save settings' }).click()
  await expect(page.getByRole('alert')).toContainText('kbps under 100')

  await page.getByLabel('Speed per connection').fill('6000')
  await page.getByLabel('Log level').selectOption('debug')
  await page.getByRole('button', { name: 'Save settings' }).click()
  await expect(page).toHaveURL(/saved=1/)
  await expect(page.getByText('Restart the relay to apply')).toBeVisible()
  await expect(page.getByLabel('Speed per connection')).toHaveValue('6000')
  await expect(page.getByLabel('Log level')).toHaveValue('debug')
})

test('sign out, a wrong password, then sign in', async () => {
  await page.getByRole('button', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login\?out=1$/)
  await page.goto('/status')
  await expect(page).toHaveURL(/\/login$/)
  await standalone('sign in')
  await page.getByLabel('Admin password').fill('not the password')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toContainText("isn't the admin password")
  await page.getByLabel('Admin password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/status$/)
})

test('the keyboard reaches everything, with a visible focus', async () => {
  await page.goto('/status')
  await page.keyboard.press('Tab')
  const skip = page.getByRole('link', { name: 'Skip to content' })
  await expect(skip).toBeFocused()
  await expect(skip).toBeInViewport()
  const reached = []
  for (let i = 0; i < 8; i++) {
    await page.keyboard.press('Tab')
    reached.push(await page.evaluate(() => {
      const el = document.activeElement
      const s = getComputedStyle(el)
      return { text: el.textContent.trim(), outline: s.outlineStyle !== 'none' && parseFloat(s.outlineWidth) >= 2 }
    }))
  }
  for (const name of ['Status', 'Health', 'Settings', 'Connect a server', 'Sign out']) {
    const hit = reached.find((r) => r.text === name)
    expect(hit, `${name} is reached by Tab`).toBeTruthy()
    expect(hit.outline, `${name} shows focus`).toBe(true)
  }
})

test('every input has a label', async () => {
  for (const p of pages) {
    await page.goto(p)
    const unlabelled = await page.evaluate(() => [...document.querySelectorAll('input:not([type=hidden]), select, textarea')]
      .filter((el) => !el.classList.contains('visually-hidden'))
      .filter((el) => !(el.labels && el.labels.length) && !el.getAttribute('aria-label'))
      .map((el) => el.name))
    expect(unlabelled, p).toEqual([])
  }
})

test('phones: no sideways scrolling on any page', async () => {
  await page.setViewportSize({ width: 360, height: 760 })
  for (const p of pages) {
    await page.goto(p)
    const wide = await page.evaluate(() => {
      const w = document.documentElement.clientWidth
      if (document.documentElement.scrollWidth <= w) return []
      return [...document.querySelectorAll('body *')]
        .filter((e) => !e.closest('.tabs, .table-wrap, pre') && e.getBoundingClientRect().right > w + 1)
        .map((e) => `${e.tagName}.${e.className} ${Math.round(e.getBoundingClientRect().right)}/${w}`)
    })
    expect(wide, `${p} scrolls sideways`).toEqual([])
  }
  await page.setViewportSize({ width: 1280, height: 800 })
})

test('dark mode follows the device', async () => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await page.goto('/status')
  const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
  expect(bg).toBe('rgb(15, 17, 21)')
  await page.emulateMedia({ colorScheme: 'light' })
  await page.reload()
  expect(await page.evaluate(() => getComputedStyle(document.body).backgroundColor)).toBe('rgb(244, 245, 248)')
})

test('a strict CSP, and nothing broke it', async () => {
  const res = await page.goto('/status')
  const csp = res.headers()['content-security-policy']
  expect(csp).toContain("default-src 'none'")
  expect(csp).not.toContain('unsafe')
  expect(problems).toEqual([])
})
