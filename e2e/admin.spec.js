// SPDX-License-Identifier: AGPL-3.0-or-later
// WittyRelay's admin page in a real browser: first run, every page, settings,
// sign out and in, the keyboard, phones, the layout, dark mode, and no CSP violations.
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

// A page with no app around it (first run, sign-in): one card with the relay's name on it, centred both ways, no bar
// above it, and nothing to scroll, at a computer's and a phone's size.
async function standalone(where) {
  for (const size of [{ width: 1280, height: 720 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(size)
    const faults = await page.evaluate(() => {
      const out = []
      const cards = [...document.querySelectorAll('main .card')]
      if (cards.length !== 1) return [`${cards.length} cards`]
      const c = cards[0].getBoundingClientRect()
      const m = document.querySelector('main').getBoundingClientRect()
      if (document.querySelector('header.top')) out.push('the app bar is drawn')
      if (!/WittyRelay/.test(cards[0].textContent.replace(/\s+/g, ''))) out.push("the card doesn't name the relay")
      if (Math.abs((c.left + c.right) / 2 - innerWidth / 2) > 2) out.push(`the card is ${Math.round((c.left + c.right) / 2 - innerWidth / 2)}px off the middle`)
      const room = m.height - c.height
      if (room > 40 && Math.abs((c.top + c.bottom) / 2 - (m.top + m.bottom) / 2) > 24) out.push(`the card is ${Math.round((c.top + c.bottom) / 2 - (m.top + m.bottom) / 2)}px off the middle of the page, top to bottom`)
      if (document.documentElement.scrollHeight > innerHeight + 1 && innerHeight >= 720 && innerWidth > 600) out.push(`it scrolls: ${document.documentElement.scrollHeight} > ${innerHeight}`)
      return out
    })
    expect(faults, `${where} at ${size.width}x${size.height}`).toEqual([])
  }
  await page.setViewportSize({ width: 1280, height: 800 })
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
  await standalone('sign-in')
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

test('layout: nothing under the top bar, each page reads from one edge', async () => {
  for (const size of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
    await page.setViewportSize(size)
    for (const p of pages) {
      await page.goto(p)
      const faults = await page.evaluate(() => {
        const out = []
        const vis = (e) => { const r = e.getBoundingClientRect(); const s = getComputedStyle(e); return r.width > 1 && r.height > 1 && s.visibility !== 'hidden' && s.display !== 'none' }
        const what = (e) => `${e.tagName.toLowerCase()} "${(e.textContent || '').trim().slice(0, 40)}"`
        const barBottom = () => document.querySelector('header.top').getBoundingClientRect().bottom
        const main = document.querySelector('main')
        // As the page opens, nothing is drawn under the bar.
        for (const e of main.querySelectorAll('h1, h2, p, a, button, input, label')) {
          const r = e.getBoundingClientRect()
          if (vis(e) && r.top < barBottom() - 1 && r.bottom > 0) out.push(`${what(e)} under the bar`)
        }
        // The title starts where the page's content starts: one edge, not a centred title over left-aligned cards.
        const h1 = main.querySelector('h1')
        const first = [...main.children].find((c) => vis(c) && !c.contains(h1) && c.getBoundingClientRect().top >= h1.getBoundingClientRect().bottom)
        if (first && Math.abs(h1.getBoundingClientRect().left - first.getBoundingClientRect().left) > 2) out.push(`the title starts ${Math.round(h1.getBoundingClientRect().left - first.getBoundingClientRect().left)}px from the content`)
        if (!['start', 'left'].includes(getComputedStyle(h1).textAlign)) out.push(`the title is ${getComputedStyle(h1).textAlign}`)
        // No heading said twice.
        const hs = [...main.querySelectorAll('h1, h2')].filter(vis).map((h) => h.textContent.trim().toLowerCase())
        for (const h of hs.filter((h, i) => hs.indexOf(h) !== i)) out.push(`"${h}" twice`)
        // A link to part of the page (the skip link, a section) lands below the bar.
        for (const id of ['main', ...[...main.querySelectorAll('section[id], h2[id]')].map((e) => e.id)]) {
          location.hash = '#' + id
          const top = document.getElementById(id).getBoundingClientRect().top
          if (top < barBottom() - 1) out.push(`#${id} lands at ${Math.round(top)}, under the bar to ${Math.round(barBottom())}`)
        }
        history.replaceState(null, '', location.pathname)
        return out
      })
      expect(faults, `${p} at ${size.width}px`).toEqual([])
    }
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
