// Receipt scanning: wires the upload and camera-capture inputs, uploads the
// image to /receipts (the server cleans it up and stores it), asks
// /receipts/{file}/analyze for the extracted items and feeds those into the
// itemization editor (via the expense-form:set-items CustomEvent) plus the
// receipt_file hidden input so the stored image attaches to the saved
// expense. The preview shows the stored image — the one the expense page
// will show later — never the raw camera output.

import { fetchJSON } from './util.js';

const section = document.getElementById('scan-section');
if (section) {
  const upload = document.getElementById('scan-upload');
  const camera = document.getElementById('scan-camera');
  const status = document.getElementById('scan-status');
  const preview = document.getElementById('scan-preview');
  const hidden = document.getElementById('receipt-file');
  // On the draft-backed new-expense form, attach the scan to the draft at
  // upload time so the image stays authorised to this user across reloads.
  const draftID = section.dataset.draft || '';

  for (const input of [upload, camera]) {
    input.addEventListener('change', () => {
      const file = input.files && input.files[0];
      if (file) scan(file);
      input.value = ''; // allow re-selecting the same file
    });
  }

  async function scan(file) {
    // Phase 1: upload. The server cleans the image up (EXIF rotation, trim,
    // deskew) and stores it.
    status.textContent = `Reading “${file.name}”…`;
    preview.hidden = true;
    preview.removeAttribute('src');

    const body = new FormData();
    body.append('image', file);
    if (draftID) body.append('draft', draftID);
    let name = '';
    try {
      const stored = await fetchJSON('/receipts', { method: 'POST', body });
      name = stored.file || '';
      hidden.value = name; // the receipt attaches even if the scan step fails

      // Phase 2: the server sends the stored image to the vision LLM.
      status.textContent = 'Processing image…';
      const result = await fetchJSON(
        `/receipts/${encodeURIComponent(name)}/analyze`, { method: 'POST' });
      showProcessed(name);
      document.dispatchEvent(new CustomEvent('expense-form:set-items', {
        detail: {
          merchant: result.merchant,
          date: result.date,
          total: toMinor(result.total),
          items: scannedItems(result),
        },
      }));
      status.textContent = result.merchant
        ? `Scanned “${result.merchant}” — check the items below.`
        : 'Scanned — check the items below.';
    } catch (err) {
      // The stored image exists even when reading it failed, so show what
      // will be saved with the expense.
      showProcessed(name);
      status.textContent = err.message || 'The scan failed. You can still add items by hand.';
    }
  }

  // The preview shows the stored, cleaned-up image — the same file the
  // expense page will display later — never the raw camera output.
  function showProcessed(name) {
    if (!name) return;
    preview.src = `/receipts/${encodeURIComponent(name)}`;
    preview.hidden = false;
  }

  // There are no separate tax/tip fields in the editor, so a receipt's tax
  // and tip come through as ordinary items (unassigned, like the rest) and
  // the rows still add up to the receipt total.
  function scannedItems(result) {
    const items = (result.items || []).map((it) => ({
      description: it.description,
      amount: toMinor(it.amount),
      people: [], // allocation is an explicit choice per item
    }));
    for (const [label, value] of [['Tax', result.tax], ['Tip', result.tip]]) {
      const minor = Math.round(Number(value) * 100);
      if (Number.isFinite(minor) && minor !== 0) {
        items.push({ description: label, amount: toMinor(value), people: [] });
      }
    }
    return items;
  }

  function toMinor(value) {
    const n = Number(value);
    if (!Number.isFinite(n)) return '';
    return String(Math.round(n * 100) / 100);
  }
}
