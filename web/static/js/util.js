// Shared helpers for the Splitpayments client modules.

export const symbol = document.documentElement.dataset.symbol || '£';

// parseAmount turns user input into integer minor units (pence), or null
// when it isn't a valid non-negative amount. Mirrors internal/money.Parse.
export function parseAmount(text) {
  if (typeof text !== 'string') return null;
  let s = text.trim().replace(/,/g, '');
  if (s.startsWith('+')) s = s.slice(1);
  const m = /^(\d*)(?:\.(\d*))?$/.exec(s);
  if (!m) return null;
  const major = m[1] === '' ? (m[2] ? 0 : null) : parseInt(m[1], 10);
  if (major === null) return null;
  let minor = m[2] || '';
  if (minor.length > 2) return null;
  while (minor.length < 2) minor += '0';
  return major * 100 + parseInt(minor || '0', 10);
}

// formatAmount renders integer minor units with the currency symbol.
export function formatAmount(minor) {
  const sign = minor < 0 ? '-' : '';
  const v = Math.abs(minor);
  return `${sign}${symbol}${Math.floor(v / 100)}.${String(v % 100).padStart(2, '0')}`;
}

// fetchJSON POSTs a body (FormData or object) and decodes a JSON response,
// throwing on non-2xx with the server's error message when available.
export async function fetchJSON(url, options = {}) {
  const response = await fetch(url, options);
  let data = null;
  try {
    data = await response.json();
  } catch {
    // non-JSON body
  }
  if (!response.ok) {
    const message = data && data.error ? data.error : `Request failed (${response.status})`;
    throw new Error(message);
  }
  return data;
}
