// The expense form editor: payers, five split modes and live validation.
//
// It takes over the #form-dynamic section of the expense form, initialised
// from the JSON payload embedded by the server in #form-payload. All state
// lives here; rows are rendered as real named inputs so a plain form POST
// carries everything back to the server, which re-validates independently.
//
// scan.js populates the itemization editor by dispatching an
// 'expense-form:set-items' CustomEvent with {items, total}; scanned tax and
// tip arrive as ordinary items in the list. The total also fills the single
// payer's amount when it is still empty.

import { parseAmount, formatAmount } from './util.js';

const payloadEl = document.getElementById('form-payload');
const dynamic = document.getElementById('form-dynamic');
if (payloadEl && dynamic) {
  init(JSON.parse(payloadEl.dataset.payload));
}

function init(payload) {
  const state = {
    members: payload.members || [],
    mode: payload.mode || 'even',
    payers: (payload.payments || []).map((p) => ({ id: p.id, amount: p.amount || '' })),
    participants: new Set(payload.participants || []),
    exact: new Map(Object.entries(payload.exact || {}).map(([k, v]) => [Number(k), v])),
    percent: new Map(Object.entries(payload.percent || {}).map(([k, v]) => [Number(k), v])),
    shares: new Map(Object.entries(payload.shares || {}).map(([k, v]) => [Number(k), v])),
    items: (payload.items || []).map((it) => ({
      description: it.description || '',
      amount: it.amount || '',
      people: new Set(it.people || []),
    })),
  };
  if (state.payers.length === 0) {
    state.payers.push({ id: state.members[0] ? state.members[0].id : 0, amount: '' });
  }

  const form = document.getElementById('expense-form');
  form.addEventListener('submit', (event) => {
    const error = validate(state);
    if (error) {
      event.preventDefault();
      showError(error);
    }
  });

  // --- draft autosave ------------------------------------------------------
  // When the form backs a server-side draft (data-autosave), every change is
  // PUT to the draft, so a refresh or process kill on mobile loses nothing.
  // The body is the same urlencoded field set the submit posts; the server
  // stores it verbatim and re-validates on render and submit.
  const autosaveURL = form.dataset.autosave;
  const draftStatus = document.getElementById('draft-status');
  let dirty = false;
  let flushing = false;
  let ready = false;

  const formBody = () => new URLSearchParams(new FormData(form)).toString();

  function setDraftStatus(text) {
    if (draftStatus) draftStatus.textContent = text;
  }

  function scheduleAutosave() {
    if (!autosaveURL || !ready) return;
    dirty = true;
    if (flushing) return; // a flush loop is already running; it picks this up
    flushing = true;
    setTimeout(flushAutosave, 400);
  }

  async function flushAutosave() {
    try {
      while (dirty) {
        dirty = false;
        setDraftStatus('Saving…');
        const res = await fetch(autosaveURL, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: formBody(),
          keepalive: true,
        });
        if (!res.ok) throw new Error(`autosave failed (${res.status})`);
        if (!dirty) setDraftStatus('Draft saved');
      }
    } catch {
      // Stay dirty: the next edit retries, and the pagehide flush below
      // makes one last attempt when the tab goes away.
      setDraftStatus('Not saved — check your connection');
    }
    flushing = false;
  }

  // Mobile PWAs kill the page without warning: grab any unsaved change at
  // the last moment. keepalive lets the request outlive the page.
  function flushNow() {
    if (!autosaveURL || !dirty) return;
    dirty = false;
    fetch(autosaveURL, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: formBody(),
      keepalive: true,
    }).catch(() => {});
  }
  window.addEventListener('pagehide', flushNow);
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') flushNow();
  });
  // Connectivity back after an offline stretch: push whatever is pending.
  window.addEventListener('online', () => {
    if (autosaveURL && dirty && !flushing) {
      flushing = true;
      flushAutosave();
    }
  });

  form.addEventListener('input', scheduleAutosave);
  form.addEventListener('change', scheduleAutosave);

  document.addEventListener('expense-form:set-items', (event) => {
    const detail = event.detail || {};
    if (Array.isArray(detail.items)) {
      state.items = detail.items.map((it) => ({
        description: it.description || '',
        amount: it.amount || '',
        people: new Set(it.people || []),
      }));
      state.mode = 'itemized';
    }
    // Only fill Amount paid for a single untouched payer row: with several
    // payers the receipt total says nothing about how the payment split.
    if (detail.total && state.payers.length === 1 && !state.payers[0].amount) {
      state.payers[0].amount = detail.total;
    }
    render();
    if (detail.merchant) {
      const desc = document.getElementById('description');
      if (desc && !desc.value) desc.value = detail.merchant;
    }
    if (detail.date) {
      const date = document.getElementById('date');
      if (date) date.value = detail.date;
    }
  });

  const memberName = (id) => {
    const m = state.members.find((m) => m.id === id);
    return m ? m.name : `#${id}`;
  };
  const memberOptions = (selected) => state.members.map((m) =>
    `<option value="${m.id}"${m.id === selected ? ' selected' : ''}>${escapeHTML(m.name)}</option>`
  ).join('');

  function total() {
    return state.payers.reduce((sum, p) => sum + (parseAmount(p.amount) || 0), 0);
  }

  // evenSplit mirrors money.Allocate: remainder to the earliest participants.
  function evenSplit(amount, ids) {
    if (ids.length === 0) return [];
    const parts = ids.map(() => Math.floor(amount / ids.length));
    let rem = amount - parts.reduce((a, b) => a + b, 0);
    for (let i = 0; rem > 0; i++, rem--) parts[i] += 1;
    return parts;
  }

  // weightedSplit mirrors money.AllocateByWeights (largest remainder).
  function weightedSplit(amount, weights) {
    const sum = weights.reduce((a, b) => a + b, 0);
    if (sum <= 0) return weights.map(() => 0);
    const parts = weights.map((w) => Math.floor((amount * w) / sum));
    let rem = amount - parts.reduce((a, b) => a + b, 0);
    const gaps = weights.map((w, i) => amount * w - parts[i] * sum);
    while (rem > 0) {
      let best = 0;
      for (let i = 1; i < gaps.length; i++) if (gaps[i] > gaps[best]) best = i;
      parts[best] += 1;
      gaps[best] -= sum;
      rem--;
    }
    return parts;
  }

  function itemPeople(item) {
    return item.people.has('all') ? state.members.map((m) => m.id) : [...item.people].map(Number);
  }

  function itemizedTotals() {
    const perUser = new Map();
    let subtotal = 0;
    for (const item of state.items) {
      const amt = item.amount.trim() === '' ? 0 : (parseAmount(item.amount) || 0);
      subtotal += amt;
      const people = itemPeople(item);
      const parts = evenSplit(amt, people);
      people.forEach((id, i) => perUser.set(id, (perUser.get(id) || 0) + parts[i]));
    }
    return { subtotal, perUser };
  }

  function validate(state) {
    if (total() <= 0) return 'The amount paid must be more than zero.';
    switch (state.mode) {
      case 'even':
        if (state.participants.size === 0) return 'Select at least one person to split between.';
        return '';
      case 'exact': {
        let sum = 0;
        for (const v of state.exact.values()) {
          const amt = v.trim() === '' ? 0 : parseAmount(v);
          if (amt === null) return 'Exact shares must be valid amounts.';
          sum += amt;
        }
        if (sum !== total()) return `Exact shares add up to ${formatAmount(sum)}, but the total is ${formatAmount(total())}.`;
        return '';
      }
      case 'percent': {
        let sum = 0;
        for (const v of state.percent.values()) {
          const p = Number(v);
          if (v === '' || Number.isNaN(p) || p < 0 || !/^\d*(?:\.\d{1,2})?$/.test(v.trim())) {
            return 'Percentages must be numbers like 33.33.';
          }
          sum += p;
        }
        if (Math.abs(sum - 100) > 0.001) return `Percentages add up to ${round2(sum)}%, not 100%.`;
        return '';
      }
      case 'shares': {
        let any = false;
        for (const v of state.shares.values()) {
          if (v !== '' && !/^\d+$/.test(v.trim())) return 'Share counts must be whole numbers.';
          if (parseInt(v, 10) > 0) any = true;
        }
        if (!any) return 'Give at least one person a share.';
        return '';
      }
      case 'itemized': {
        if (state.items.length === 0) return 'Add at least one item.';
        for (const item of state.items) {
          if (!item.description.trim()) return 'Every item needs a description.';
          if (item.amount.trim() !== '' && parseAmount(item.amount) === null) return 'Item amounts must be valid.';
          if (item.people.size === 0) return 'Assign every item to at least one person.';
        }
        const { subtotal } = itemizedTotals();
        if (subtotal !== total()) {
          return `Items (${formatAmount(subtotal)}) must equal the amount paid (${formatAmount(total())}).`;
        }
        return '';
      }
    }
    return 'Unknown split mode.';
  }

  function remainingHTML() {
    const tot = total();
    switch (state.mode) {
      case 'even': {
        const ids = [...state.participants];
        if (tot <= 0 || ids.length === 0) return '';
        const parts = evenSplit(tot, ids);
        return ids.map((id, i) => `${escapeHTML(memberName(id))}: ${formatAmount(parts[i])}`).join(' · ');
      }
      case 'exact': {
        let sum = 0;
        for (const v of state.exact.values()) sum += (v.trim() === '' ? 0 : parseAmount(v)) || 0;
        return remainingPiece(tot - sum, tot, false);
      }
      case 'percent': {
        let sum = 0;
        for (const v of state.percent.values()) sum += Number(v) || 0;
        const rem = round2(100 - sum);
        return remainingPiece(rem, 0, true);
      }
      case 'shares': {
        const entries = [...state.shares.entries()]
          .filter(([, v]) => parseInt(v, 10) > 0)
          .sort((a, b) => a[0] - b[0]);
        if (tot <= 0 || entries.length === 0) return '';
        const parts = weightedSplit(tot, entries.map(([, v]) => parseInt(v, 10)));
        return entries.map(([id], i) => `${escapeHTML(memberName(id))}: ${formatAmount(parts[i])}`).join(' · ');
      }
      case 'itemized': {
        const { subtotal, perUser } = itemizedTotals();
        const rem = tot - subtotal;
        let assigned = 0;
        const people = [...perUser.keys()].sort((a, b) => a - b)
          .map((id) => {
            assigned += perUser.get(id);
            return `${escapeHTML(memberName(id))}: ${formatAmount(perUser.get(id))}`;
          })
          .join(' · ');
        const unassigned = subtotal - assigned;
        return `Items: ${formatAmount(subtotal)}` +
          (people ? ` · ${people}` : '') +
          (unassigned > 0 ? ` · <span class="remaining-bad">unassigned: ${formatAmount(unassigned)}</span>` : '') +
          ' · ' + remainingPiece(rem, tot, false);
      }
    }
    return '';
  }

  function remainingPiece(rem, tot, isPercent) {
    if (isPercent) {
      if (Math.abs(rem) < 0.001) {
        return '<span class="remaining-ok">remaining: 0%</span>';
      }
      return `<span class="remaining-bad">remaining: ${round2(rem)}%</span>`;
    }
    if (tot <= 0) {
      return '<span class="remaining-bad">no amount paid yet</span>';
    }
    if (Math.abs(rem) < 1) {
      return `<span class="remaining-ok">remaining: ${formatAmount(0)}</span>`;
    }
    return `<span class="remaining-bad">remaining: ${formatAmount(rem)}</span>`;
  }

  function showError(message) {
    let el = document.getElementById('form-error');
    if (!el) {
      el = document.createElement('p');
      el.className = 'form-error';
      el.id = 'form-error';
      form.prepend(el);
    }
    el.textContent = message;
    el.scrollIntoView({ block: 'nearest' });
  }

  // --- rendering -----------------------------------------------------------

  function render() {
    dynamic.replaceChildren();

    const modeInput = document.createElement('input');
    modeInput.type = 'hidden';
    modeInput.name = 'split_mode';
    modeInput.value = state.mode;
    dynamic.append(modeInput);

    dynamic.append(payersFieldset());
    dynamic.append(splitFieldset());

    const hint = document.getElementById('remaining-hint');
    if (hint) hint.innerHTML = remainingHTML();
    // Mode switches and row add/remove never fire input events.
    scheduleAutosave();
  }

  function payersFieldset() {
    const fs = el('fieldset');
    fs.append(el('legend', 'Paid by'));
    state.payers.forEach((payer, i) => {
      const row = el('div', 'payer-row');
      const select = document.createElement('select');
      select.name = `payer_id_${i}`;
      select.setAttribute('aria-label', 'Who paid');
      select.innerHTML = memberOptions(payer.id);
      select.value = String(payer.id);
      select.addEventListener('change', () => { payer.id = Number(select.value); });
      const amount = document.createElement('input');
      amount.type = 'text';
      amount.name = `payer_amount_${i}`;
      amount.inputMode = 'decimal';
      amount.placeholder = '0.00';
      amount.value = payer.amount;
      amount.setAttribute('aria-label', 'Amount paid');
      amount.addEventListener('input', () => { payer.amount = amount.value; updateHint(); });
      const remove = removeButton(() => {
        state.payers.splice(i, 1);
        render();
      });
      row.append(select, amount, remove);
      fs.append(row);
    });
    const add = el('button', 'add-link');
    add.type = 'button';
    add.textContent = '+ Add another payer';
    add.addEventListener('click', () => {
      const used = new Set(state.payers.map((p) => p.id));
      const next = state.members.find((m) => !used.has(m.id)) || state.members[0];
      state.payers.push({ id: next ? next.id : 0, amount: '' });
      render();
    });
    fs.append(add);
    return fs;
  }

  function splitFieldset() {
    const fs = el('fieldset');
    const tabs = el('div', 'mode-tabs');
    for (const [mode, label] of [
      ['even', 'Evenly'], ['exact', 'Exact amounts'], ['percent', 'Percentages'],
      ['shares', 'Shares'], ['itemized', 'Items'],
    ]) {
      const tab = el('button', 'mode-tab');
      tab.type = 'button';
      tab.textContent = label;
      tab.setAttribute('aria-pressed', String(state.mode === mode));
      tab.addEventListener('click', () => { state.mode = mode; render(); });
      tabs.append(tab);
    }
    fs.append(tabs, modeBody());
    return fs;
  }

  function modeBody() {
    switch (state.mode) {
      case 'even': return evenBody();
      case 'exact': return perMemberBody('exact', 'Exact amounts', (m) => state.exact.get(m.id) || '', (m, v) => state.exact.set(m.id, v));
      case 'percent': return percentBody();
      case 'shares': return sharesBody();
      case 'itemized': return itemizedBody();
    }
    return document.createDocumentFragment();
  }

  function evenBody() {
    const box = el('div', 'checkbox-grid');
    for (const m of state.members) {
      const label = el('label', 'checkbox');
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.name = `participant_${m.id}`;
      cb.checked = state.participants.has(m.id);
      cb.addEventListener('change', () => {
        if (cb.checked) state.participants.add(m.id);
        else state.participants.delete(m.id);
        updateHint();
      });
      label.append(cb, document.createTextNode(m.name));
      box.append(label);
    }
    return box;
  }

  function perMemberBody(mode, legendLabel, getter, setter) {
    const box = el('div');
    for (const m of state.members) {
      const row = el('div', 'split-row split-even');
      const name = el('span', null, m.name);
      const input = document.createElement('input');
      input.type = 'text';
      input.name = `${mode}_${m.id}`;
      input.inputMode = 'decimal';
      input.placeholder = mode === 'percent' ? '0.00%' : mode === 'shares' ? '1' : '0.00';
      input.value = getter(m);
      input.setAttribute('aria-label', `${m.name} ${legendLabel.toLowerCase()}`);
      input.addEventListener('input', () => { setter(m, input.value); updateHint(); });
      row.append(name, input);
      box.append(row);
    }
    return box;
  }

  function percentBody() {
    const box = el('div');
    box.append(perMemberBody('percent', 'Percentages', (m) => state.percent.get(m.id) || '', (m, v) => state.percent.set(m.id, v)));
    const hint = el('p', 'hint', 'Enter percentages that add up to exactly 100%.');
    box.append(hint);
    return box;
  }

  function sharesBody() {
    const box = el('div');
    box.append(perMemberBody('shares', 'Shares', (m) => state.shares.get(m.id) || '', (m, v) => state.shares.set(m.id, v)));
    box.append(el('p', 'hint', 'Split in proportion to whole-number shares, e.g. 1 for yourself and 2 for a couple.'));
    return box;
  }

  function itemizedBody() {
    const box = el('div');
    const table = el('table', 'item-table');
    table.innerHTML = '<thead><tr><th>Item</th><th>Amount</th><th>Assign to</th><th></th></tr></thead>';
    const tbody = document.createElement('tbody');

    state.items.forEach((item, i) => {
      const tr = document.createElement('tr');
      const desc = document.createElement('input');
      desc.type = 'text';
      desc.name = `item_desc_${i}`;
      desc.placeholder = 'Description';
      desc.value = item.description;
      desc.addEventListener('input', () => { item.description = desc.value; });

      const amount = document.createElement('input');
      amount.type = 'text';
      amount.name = `item_amount_${i}`;
      amount.inputMode = 'decimal';
      amount.placeholder = '0.00';
      amount.value = item.amount;
      amount.addEventListener('input', () => { item.amount = amount.value; updateHint(); });

      const people = el('div', 'item-people');
      // 'all' is the only representation of "everyone"; individual ids mean a
      // narrowed assignment, even when every member happens to be ticked.
      const all = item.people.has('all');
      const allLabel = el('label', 'checkbox');
      const allCb = document.createElement('input');
      allCb.type = 'checkbox';
      allCb.name = `item_people_${i}`;
      allCb.value = 'all';
      allCb.checked = all;
      allCb.addEventListener('change', () => {
        if (allCb.checked) item.people = new Set(['all']);
        // Unchecking narrows from everyone: seed every member so the item
        // stays fully allocated while individual boxes are unticked.
        else item.people = new Set(state.members.map((m) => String(m.id)));
        render();
      });
      allLabel.append(allCb, document.createTextNode('Everyone'));
      people.append(allLabel);
      if (!allCb.checked) {
        for (const m of state.members) {
          const label = el('label', 'checkbox');
          const cb = document.createElement('input');
          cb.type = 'checkbox';
          cb.name = `item_people_${i}`;
          cb.value = String(m.id);
          cb.checked = !all && item.people.has(String(m.id));
          cb.addEventListener('change', () => {
            if (cb.checked) item.people.add(String(m.id));
            else item.people.delete(String(m.id));
            updateHint();
          });
          label.append(cb, document.createTextNode(m.name));
          people.append(label);
        }
      }

      const remove = document.createElement('td');
      remove.append(removeButton(() => {
        state.items.splice(i, 1);
        render();
      }));

      const tdDesc = document.createElement('td'); tdDesc.append(desc);
      const tdAmount = document.createElement('td'); tdAmount.append(amount);
      const tdPeople = document.createElement('td'); tdPeople.append(people);
      tr.append(tdDesc, tdAmount, tdPeople, remove);
      tbody.append(tr);
    });

    table.append(tbody);
    box.append(table);

    const add = el('button', 'add-link');
    add.type = 'button';
    add.textContent = '+ Add item';
    add.addEventListener('click', () => {
      state.items.push({ description: '', amount: '', people: new Set() });
      render();
    });
    box.append(add);
    return box;
  }

  function removeButton(onClick) {
    const btn = el('button', 'remove');
    btn.type = 'button';
    btn.textContent = '×';
    btn.setAttribute('aria-label', 'Remove');
    btn.addEventListener('click', onClick);
    return btn;
  }

  function updateHint() {
    const hint = document.getElementById('remaining-hint');
    if (hint) hint.innerHTML = remainingHTML();
  }

  render();
  ready = true; // the initial render must not autosave the untouched form
}

function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function escapeHTML(s) {
  return s.replace(/[&<>"']/g, (c) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  }[c]));
}

function round2(v) {
  return Math.round(v * 100) / 100;
}
