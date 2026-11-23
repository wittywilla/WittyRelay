// SPDX-License-Identifier: AGPL-3.0-or-later
// WittyRelay's admin page: keeps Status current and makes Copy buttons work.
// Everything works without it; it only saves reloading.
(() => {
  'use strict'

  // Copy buttons: data-copy="text", or data-copy-from="id of an element".
  document.addEventListener('click', async (e) => {
    const b = e.target.closest('[data-copy], [data-copy-from]')
    if (!b) return
    const from = b.dataset.copyFrom && document.getElementById(b.dataset.copyFrom)
    const text = from ? from.textContent : b.dataset.copy
    const was = b.textContent
    try {
      await navigator.clipboard.writeText(text)
      b.textContent = 'Copied'
    } catch {
      b.textContent = 'Select and copy'
    }
    setTimeout(() => { b.textContent = was }, 1600)
  })

  if (!document.querySelector('[data-spark]')) return

  const rows = (tbody, list) => {
    const cols = tbody.dataset.cols.split(' ')
    const frag = document.createDocumentFragment()
    if (!list.length) {
      const tr = document.createElement('tr')
      const td = document.createElement('td')
      td.colSpan = cols.length
      td.className = 'empty'
      td.textContent = tbody.dataset.empty
      tr.append(td)
      frag.append(tr)
    }
    for (const item of list) {
      const tr = document.createElement('tr')
      cols.forEach((c, i) => {
        const td = document.createElement('td')
        if (i > 0) td.className = 'num'
        if (c === 'id') {
          const code = document.createElement('code')
          code.textContent = item[c]
          td.append(code)
        } else {
          td.textContent = item[c]
        }
        tr.append(td)
      })
      frag.append(tr)
    }
    tbody.replaceChildren(frag)
  }

  let failed = 0
  const tick = async () => {
    if (document.hidden) return
    try {
      const r = await fetch('/api/status', { headers: { Accept: 'application/json' }, cache: 'no-store' })
      if (r.status === 401) { location.assign('/login'); return }
      if (!r.ok) throw new Error(String(r.status))
      const d = await r.json()
      for (const el of document.querySelectorAll('[data-k]')) {
        const v = d[el.dataset.k]
        if (typeof v === 'string' && el.textContent !== v) el.textContent = v
      }
      for (const el of document.querySelectorAll('[data-state]')) el.dataset.state = d.up ? 'up' : 'down'
      for (const el of document.querySelectorAll('[data-health]')) el.dataset.health = d.healthState
      document.querySelector('[data-spark]').setAttribute('points', d.spark)
      for (const tb of document.querySelectorAll('tbody[data-rows]')) rows(tb, d[tb.dataset.rows] || [])
      failed = 0
      const live = document.querySelector('[data-live]')
      if (live) live.textContent = 'Updates every 5 seconds.'
    } catch {
      failed++
      const live = document.querySelector('[data-live]')
      if (live && failed > 1) live.textContent = 'Can’t reach the relay. Retrying…'
    }
  }
  setInterval(tick, 5000)
  document.addEventListener('visibilitychange', () => { if (!document.hidden) tick() })
})()
