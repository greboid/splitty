// Small page-level behaviours: confirm dialogs for destructive forms,
// plus service-worker registration for offline/installable (PWA) support.

// This module is loaded with ?v=<asset version>, which the service worker
// reuses as its cache version: a rebuild changes the worker's URL, forcing
// a reinstall and a clean cache.
const assetVersion = new URL(import.meta.url).searchParams.get('v');

if ('serviceWorker' in navigator) {
  window.addEventListener('load', () => {
    navigator.serviceWorker.register(`/sw.js?v=${assetVersion}`)
      .catch((err) => console.error('service worker registration failed', err));
  });
}

const forms = document.querySelectorAll('form.js-confirm[data-confirm]');

// Mobile drawer: the sidebar slides in behind the topbar ☰ toggle.
const sidebar = document.getElementById('sidebar');
const navToggle = document.querySelector('.nav-toggle');
const backdrop = document.getElementById('backdrop');

if (sidebar && navToggle) {
  const setOpen = (open) => {
    document.body.classList.toggle('sidebar-open', open);
    navToggle.setAttribute('aria-expanded', String(open));
  };
  navToggle.addEventListener('click', () => setOpen(!document.body.classList.contains('sidebar-open')));
  backdrop?.addEventListener('click', () => setOpen(false));
  sidebar.addEventListener('click', (event) => {
    // Navigate and put the drawer away in one tap.
    if (event.target.closest('a')) setOpen(false);
  });
}

for (const form of forms) {
  form.addEventListener('submit', (event) => {
    if (form.dataset.confirmed === 'true') return; // second pass: really submit
    event.preventDefault();
    confirmDialog(form.dataset.confirm)
      .then((ok) => {
        if (ok) {
          form.dataset.confirmed = 'true';
          form.submit();
        }
      });
  });
}

function confirmDialog(message) {
  return new Promise((resolve) => {
    const dialog = document.createElement('dialog');
    dialog.className = 'confirm-dialog';
    const text = document.createElement('p');
    text.textContent = message;
    const buttons = document.createElement('div');
    buttons.className = 'confirm-buttons';
    const cancel = document.createElement('button');
    cancel.type = 'button';
    cancel.className = 'btn btn-ghost';
    cancel.textContent = 'Cancel';
    const ok = document.createElement('button');
    ok.type = 'button';
    ok.className = 'btn btn-danger';
    ok.textContent = 'Confirm';
    buttons.append(cancel, ok);
    dialog.append(text, buttons);
    document.body.append(dialog);

    const done = (result) => {
      dialog.close();
      resolve(result);
    };
    cancel.addEventListener('click', () => done(false));
    ok.addEventListener('click', () => done(true));
    dialog.addEventListener('cancel', () => done(false));
    dialog.showModal();
  });
}
