(() => {
  const browserTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const padTime = value => String(value).padStart(2, '0');
  function localizeTimes(root = document) {
    for (const node of root.querySelectorAll('time[data-unix]')) {
      const date = new Date(Number(node.dataset.unix) * 1000);
      if (!Number.isFinite(date.getTime())) continue;
      node.textContent = `${String(date.getFullYear()).padStart(4, '0')}.${padTime(date.getMonth() + 1)}.${padTime(date.getDate())} ${padTime(date.getHours())}:${padTime(date.getMinutes())}:${padTime(date.getSeconds())}`;
      node.title = `${browserTimeZone} · ${date.toISOString()}`;
      node.dataset.timeZone = browserTimeZone;
    }
  }
  localizeTimes();
  const fragments = ['summary', 'feed-list', 'git-feed-list', 'recent-syncs', 'reader-sessions', 'browser-sessions', 'security-events', 'storage-footer'];
  const filter = document.querySelector('#disabled-only');
  if (!filter) return;
  let requestNumber = 0;
  let refreshController;
  let pending = 0;
  let noticeTimer;

  function notify(message, error = false) {
    const node = document.querySelector('#notice');
    clearTimeout(noticeTimer);
    node.textContent = message;
    node.classList.toggle('notice-error', error);
    node.hidden = false;
    noticeTimer = setTimeout(() => { node.hidden = true; }, error ? 12000 : 5000);
  }
  function expired(response) {
    if (response.status !== 401) return false;
    window.location.assign('/login');
    return true;
  }
  async function refresh(background = false) {
    const number = ++requestNumber;
    if (refreshController) refreshController.abort();
    refreshController = new AbortController();
    const url = new URL('/', window.location.origin);
    for (const key of ['reader_page', 'browser_page']) {
      const value = new URL(window.location.href).searchParams.get(key);
      if (value) url.searchParams.set(key, value);
    }
    if (filter.checked) url.searchParams.set('disabled', '1');
    url.searchParams.set('fragment', '1');
    try {
      const response = await fetch(url, { headers: { 'X-ROI-Request': 'async' }, signal: refreshController.signal });
      if (expired(response)) return;
      if (!response.ok) throw new Error(await response.text());
      const parsed = new DOMParser().parseFromString(await response.text(), 'text/html');
      localizeTimes(parsed);
      if (number !== requestNumber) return;
      // Validate the entire fragment response before updating any section.
      if (fragments.some(id => !parsed.getElementById(id))) throw new Error('The dashboard response was incomplete.');
      const positions = ['feed-scroll', 'git-feed-scroll', 'sync-scroll', 'reader-session-scroll', 'browser-session-scroll', 'security-scroll'].map(id => {
        const node = document.getElementById(id);
        return { id, left: node.scrollLeft, top: node.scrollTop };
      });
      const top = window.scrollY;
      for (const id of fragments) document.getElementById(id).replaceWith(document.importNode(parsed.getElementById(id), true));
      for (const position of positions) {
        const node = document.getElementById(position.id);
        node.scrollLeft = position.left;
        node.scrollTop = position.top;
      }
      window.scrollTo(0, top);
    } catch (error) {
      if (error.name !== 'AbortError' && !background) notify(error.message || 'Unable to refresh the dashboard.', true);
    }
  }
  function legacyCopy(value) {
    const active = document.activeElement;
    const input = document.createElement('textarea');
    input.className = 'copy-buffer';
    input.value = value;
    input.readOnly = true;
    input.setAttribute('aria-hidden', 'true');
    (active?.closest('dialog[open]') || document.body).append(input);
    try {
      input.select();
      if (!document.execCommand('copy')) throw new Error('Copy failed. Select the URL and copy it manually.');
    } finally {
      input.remove();
      if (active?.isConnected) active.focus({ preventScroll: true });
    }
  }
  async function copyFeedURL(button) {
    const value = button.closest('[data-feed-id]').dataset.repository;
    try {
      if (navigator.clipboard?.writeText) {
        try { await navigator.clipboard.writeText(value); } catch { legacyCopy(value); }
      } else legacyCopy(value);
      button.classList.add('is-copied');
      button.title = 'Copied!';
      notify('URL copied.');
      setTimeout(() => { button.classList.remove('is-copied'); button.title = 'Copy URL'; }, 1800);
    } catch (error) { notify(error.message || 'Unable to copy the URL.', true); }
  }
  function closeDialog(dialog) {
    dialog.close();
    const error = dialog.querySelector('.dialog-error');
    if (error) { error.hidden = true; error.textContent = ''; }
  }
  document.querySelector('#api-credentials-dialog').addEventListener('close', () => {
    document.querySelector('#api-reader-url').value = '';
    document.querySelector('#api-personal-login').value = '';
    document.querySelector('#api-sign-in-code').value = '';
  });
  let pendingImport = null;
  async function previewImport(form) {
    const upload = new FormData(form);
    const format = new URL(form.action).pathname === '/import-roi' ? 'roi' : 'opml';
    const body = new FormData();
    body.set('csrf', upload.get('csrf'));
    body.set('format', format);
    body.set('file', upload.get(format));
    const response = await fetch('/preview-import', { method: 'POST', body, headers: { Accept: 'application/json', 'X-ROI-Request': 'async' } });
    if (expired(response)) return;
    if (!response.ok) throw new Error((await response.text()).trim() || 'Unable to preview import.');
    const preview = await response.json();
    document.querySelector('#import-summary').textContent = `${preview.total} subscriptions: ${preview.add} to add, ${preview.skipped} duplicates to skip, ${preview.invalid} invalid.`;
    const rows = document.querySelector('#import-items');
    rows.replaceChildren();
    for (const item of preview.items) {
      const row = document.createElement('tr');
      for (const text of [item.index, item.type, item.title, item.url, item.error || (item.status === 'skip' ? 'Skip duplicate' : 'Add')]) {
        const cell = document.createElement('td'); cell.textContent = text; row.append(cell);
      }
      if (item.status === 'invalid') row.classList.add('has-error');
      rows.append(row);
    }
    document.querySelector('#confirm-import').disabled = preview.invalid > 0;
    pendingImport = form;
    document.querySelector('#import-preview-dialog').showModal();
  }
  document.querySelector('#confirm-import').addEventListener('click', () => {
    const form = pendingImport;
    if (!form) return;
    pendingImport = null;
    closeDialog(document.querySelector('#import-preview-dialog'));
    form.dataset.importConfirmed = 'true';
    form.requestSubmit();
  });
  document.addEventListener('submit', async event => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement) || !form.hasAttribute('data-async')) return;
    event.preventDefault();
    if (form.dataset.busy === 'true' || !form.reportValidity()) return;
    if (form.hasAttribute('data-confirm-device-action')) {
      const dialog = document.querySelector('#api-device-confirm-dialog');
      const confirmation = dialog.querySelector('form');
      const remove = new URL(form.action).pathname === '/api-devices/delete';
      confirmation.action = form.action;
      confirmation.querySelector('input[name=device]').value = form.querySelector('input[name=device]').value;
      dialog.querySelector('h2').textContent = remove ? 'Delete API device?' : 'Generate a new sign-in code?';
      dialog.querySelector('.api-device-name').textContent = form.closest('[data-api-device-id]').dataset.deviceName;
      dialog.querySelector('.api-device-description').textContent = remove ? 'Deletes this API access and revokes all of its sessions. Subscriptions and reading state are preserved.' : 'Replaces any unused code. Existing sessions keep working. Each new session requires approval.';
      dialog.showModal();
      return;
    }
    if (form.hasAttribute('data-confirm-revoke')) {
      const dialog = document.querySelector('#revoke-dialog');
      const confirmation = dialog.querySelector('form');
      confirmation.action = form.action;
      confirmation.querySelector('input[name=session]').value = form.querySelector('input[name=session]')?.value || '';
      const path = new URL(form.action).pathname;
      const row = form.closest('[data-session-id]');
      const browserSession = path === '/revoke-browser-sessions' || row?.dataset.sessionRealm === 'admin';
      dialog.querySelector('h2').textContent = path === '/revoke-api-tokens' ? 'Revoke all Reader API sessions?' : path === '/revoke-browser-sessions' ? 'Revoke all browser sessions?' : browserSession ? 'Revoke browser session?' : 'Revoke Reader API session?';
      dialog.querySelector('#revoke-description').textContent = browserSession ? 'The browser must sign in again. Revoking the current session signs you out.' : path === '/revoke-api-tokens' ? 'Revokes all Reader API sessions and unused sign-in codes. Generate new codes to reconnect; every new session requires approval.' : 'The Reader API client must sign in again with a new one-time code. Its new session requires approval.';
      dialog.querySelector('.revoke-name').textContent = row ? (row.dataset.deviceName || (browserSession ? 'Browser session' : 'Unapproved Reader client')) + ' · ' + row.dataset.sessionId.slice(0, 12) : browserSession ? 'All browsers, including this one, will be signed out.' : 'All Reader clients will be signed out.';
      dialog.showModal();
      return;
    }
    if (['/import', '/import-roi'].includes(new URL(form.action).pathname) && form.dataset.importConfirmed !== 'true') {
      form.dataset.busy = 'true';
      pending++;
      try { await previewImport(form); } catch (error) { notify(error.message || 'Unable to preview import.', true); }
      finally { delete form.dataset.busy; pending--; }
      return;
    }
    delete form.dataset.importConfirmed;
    const data = new FormData(form);
    const body = form.enctype === 'multipart/form-data' ? data : new URLSearchParams(data);
    const buttons = [...form.querySelectorAll('button')];
    form.dataset.busy = 'true';
    form.setAttribute('aria-busy', 'true');
    buttons.forEach(button => { button.disabled = true; });
    pending++;
    try {
      const response = await fetch(form.action, { method: 'POST', body, headers: { Accept: 'application/json', 'X-ROI-Request': 'async' } });
      if (expired(response)) return;
      if (!response.ok) throw new Error((await response.text()).trim() || 'The action failed.');
      const result = await response.json();
      if (result.redirect) { window.location.assign(result.redirect); return; }
      const dialog = form.closest('dialog');
      if (dialog) closeDialog(dialog);
      if (['/import', '/import-roi'].includes(new URL(form.action).pathname)) form.reset();
      await refresh();
      if (result.credentials) {
        document.querySelector('#api-reader-url').value = result.credentials.api_url || '';
        document.querySelector('#api-url-fields').hidden = !result.credentials.api_url;
        document.querySelector('#api-personal-login').value = result.credentials.login;
        document.querySelector('#api-sign-in-code').value = result.credentials.code;
        document.querySelector('#api-credentials-dialog').showModal();
      }
      notify(result.message || 'Saved.');
    } catch (error) {
      const dialogError = form.closest('dialog')?.querySelector('.dialog-error');
      if (dialogError) { dialogError.textContent = error.message; dialogError.hidden = false; }
      notify(error.message || 'The action failed.', true);
    } finally {
      pending--;
      delete form.dataset.busy;
      form.removeAttribute('aria-busy');
      buttons.forEach(button => { button.disabled = false; });
    }
  });
  document.addEventListener('click', event => {
    const credentialCopy = event.target.closest('[data-copy-credential]');
    if (credentialCopy) {
      const value = document.getElementById(credentialCopy.dataset.copyCredential).value;
      (async () => {
        try { if (navigator.clipboard?.writeText) { try { await navigator.clipboard.writeText(value); } catch { legacyCopy(value); } } else legacyCopy(value); notify('Copied.'); }
        catch { notify('Unable to copy. Select the value and copy it manually.', true); }
      })();
      return;
    }
    const addDevice = event.target.closest('[data-api-device-add]');
    const editDevice = event.target.closest('[data-api-device-edit]');
    if (addDevice || editDevice) {
      const dialog = document.querySelector('#api-device-dialog');
      const form = dialog.querySelector('form');
      const row = editDevice?.closest('[data-api-device-id]');
      form.action = row ? '/api-devices/rename' : '/api-devices/create';
      form.querySelector('[name=device]').value = row?.dataset.apiDeviceId || '';
      form.querySelector('[name=device_name]').value = row?.dataset.deviceName || '';
      dialog.querySelector('h2').textContent = row ? 'Rename API device' : 'Add API device';
      form.querySelector('[type=submit]').textContent = row ? 'Save name' : 'Create device';
      dialog.showModal();
      return;
    }
    const copy = event.target.closest('[data-copy-url]');
    if (copy) { event.preventDefault(); copyFeedURL(copy); return; }
    const reset = event.target.closest('[data-reset]');
    if (reset) {
      event.preventDefault();
      const row = reset.closest('[data-feed-id]');
      const dialog = document.querySelector('#reset-dialog');
      dialog.querySelector('input[name=id]').value = row.dataset.feedId;
      dialog.querySelector('.reset-name').textContent = row.dataset.title;
      dialog.querySelector('.reset-url').textContent = row.dataset.repository;
      dialog.showModal();
      return;
    }
    const rssSettings = event.target.closest('[data-rss-settings]');
    if (rssSettings) {
      event.preventDefault();
      const row = rssSettings.closest('[data-feed-id]');
      const dialog = document.querySelector('#rss-settings-dialog');
      dialog.querySelector('input[name=id]').value = row.dataset.feedId;
      dialog.querySelector('.rss-settings-name').textContent = row.dataset.title;
      const input = dialog.querySelector('input[name=user_agent]');
      input.value = row.dataset.userAgent;
      dialog.showModal();
      input.focus();
      return;
    }
    const gitAdd = event.target.closest('[data-git-add]');
    const gitEdit = event.target.closest('[data-git-edit]');
    if (gitAdd || gitEdit) {
      event.preventDefault();
      const row = gitEdit?.closest('[data-feed-id]');
      const dialog = document.querySelector('#git-dialog');
      const form = dialog.querySelector('form');
      form.reset();
      form.querySelector('input[name=id]').value = row ? row.dataset.feedId : '0';
      const input = form.querySelector('input[name=repository]');
      input.value = row ? row.dataset.repository : '';
      input.readOnly = !!row;
      const provider = form.querySelector('select[name=provider]');
      provider.value = row ? row.dataset.gitProvider : 'auto';
      provider.disabled = !!row;
      const track = form.querySelector('select[name=mode]');
      const updateGitMode = () => {
        const gitweb = provider.value === 'gitweb' || (provider.value === 'auto' && /[?&;]p=/.test(input.value));
        track.querySelector('option[value=releases]').disabled = gitweb;
        if (gitweb) track.value = 'tags';
      };
      provider.onchange = updateGitMode;
      input.oninput = updateGitMode;
      form.querySelector('select[name=mode]').value = row ? row.dataset.gitMode : 'releases';
      form.querySelector('input[name=tag_filter]').value = row?.dataset.gitTagFilter || '';
      form.querySelector('input[name=include_preview]').checked = row?.dataset.gitPreview === 'true';
      updateGitMode();
      dialog.querySelector('h2').textContent = row ? 'Git source settings' : 'Add Git repository';
      dialog.showModal();
      if (!row) input.focus();
      return;
    }
    const approve = event.target.closest('[data-device-approve]');
    const deviceEdit = event.target.closest('[data-device-edit]');
    if (approve || deviceEdit) {
      event.preventDefault();
      const row = (approve || deviceEdit).closest('[data-session-id]');
      const dialog = document.querySelector('#device-dialog');
      const form = dialog.querySelector('form');
      form.action = approve ? '/approve-session' : '/rename-device';
      form.querySelector('input[name=session]').value = row.dataset.sessionId;
      const input = form.querySelector('input[name=device_name]');
      input.value = row.dataset.deviceName;
      dialog.querySelector('h2').textContent = approve ? 'Approve device' : 'Edit device name';
      form.querySelector('button[type=submit]').textContent = approve ? 'Approve device' : 'Save device name';
      dialog.showModal();
      input.focus();
      input.select();
      return;
    }
    const edit = event.target.closest('[data-rename]');
    const remove = event.target.closest('[data-delete]');
    const close = event.target.closest('[data-close-dialog]');
    if (close) { closeDialog(close.closest('dialog')); return; }
    if (!edit && !remove) return;
    event.preventDefault();
    const row = (edit || remove).closest('[data-feed-id]');
    const dialog = document.querySelector(edit ? '#rename-dialog' : '#delete-dialog');
    dialog.querySelector('input[name=id]').value = row.dataset.feedId;
    if (edit) dialog.querySelector('input[name=title]').value = row.dataset.title;
    else dialog.querySelector('.delete-name').textContent = row.dataset.title;
    dialog.showModal();
    if (edit) { const input = dialog.querySelector('input[name=title]'); input.focus(); input.select(); }
  });
  filter.addEventListener('change', () => {
    const url = new URL(window.location.href);
    if (filter.checked) url.searchParams.set('disabled', '1'); else url.searchParams.delete('disabled');
    history.replaceState(null, '', url);
    refresh();
  });
  document.addEventListener('click', event => {
    const link = event.target.closest('[data-session-page]');
    if (!link) return;
    event.preventDefault();
    history.replaceState(null, '', link.href);
    refresh();
  });
  document.addEventListener('visibilitychange', () => { if (!document.hidden && !pending) refresh(true); });
  setInterval(() => { if (!document.hidden && !pending) refresh(true); }, 5000);
})();
