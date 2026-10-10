'use strict';
// Cellular: connection details, 5G mode and the band editor.

Views.cellular = { sources: ['cellular', 'connectivity', 'status', 'netcfg', 'pin', 'autopin', 'apn'] };

// The main commercial LTE / 5G NR deployments per region (GSMA / 3GPP
// allocations cross-checked against real operators). Presets are always
// intersected with what the modem supports.
const NR_REGIONS = {
  Europe:  [1, 3, 7, 8, 20, 28, 38, 40, 75, 76, 77, 78],
  America: [2, 5, 12, 14, 25, 26, 28, 29, 30, 41, 48, 66, 70, 71, 77, 78],
  Asia:    [1, 3, 5, 8, 28, 40, 41, 77, 78, 79],
};
const LTE_REGIONS = {
  Europe:  [1, 3, 7, 8, 20, 28, 32, 38, 40, 42, 43],
  America: [2, 4, 5, 7, 12, 13, 14, 17, 25, 26, 28, 29, 30, 41, 46, 48, 66, 71],
  Asia:    [1, 3, 5, 7, 8, 11, 18, 19, 21, 26, 28, 34, 38, 39, 40, 41, 42],
};
const REGIONS = ['Europe', 'America', 'Asia', 'All'];
const unionOf = o => Array.from(new Set(Object.values(o).flat())).sort((a, b) => a - b);
const parseBands = s => (s || '').split(/[:,]/).filter(Boolean).map(Number);
const joinBands = a => Array.from(new Set(a)).sort((x, y) => x - y).join(':');

const GROUPS = [
  { key: 'nr5g_band', el: 'bandsSA', prefix: 'n', kind: 'nr', label: 'SA' },
  { key: 'nsa_nr5g_band', el: 'bandsNSA', prefix: 'n', kind: 'nr', label: 'NSA' },
  { key: 'lte_band', el: 'bandsLTE', prefix: 'B', kind: 'lte', label: 'LTE' },
];

const Bands = {
  configured: {}, effective: {}, hwNr: null, hwLte: null, model: '',
  edit: {}, dirty: false, live: { nr: new Set(), lte: new Set(), rat: '' },
};

function universe(kind) {
  const hw = kind === 'lte' ? Bands.hwLte : Bands.hwNr;
  return hw || unionOf(kind === 'lte' ? LTE_REGIONS : NR_REGIONS);
}
function presetFor(region, kind) {
  const u = universe(kind);
  if (region === 'All') return u.slice();
  return (kind === 'lte' ? LTE_REGIONS : NR_REGIONS)[region].filter(b => u.includes(b));
}

function setBandsDirty(d) {
  Bands.dirty = d;
  $('#bandsApply').disabled = !d;
  $('#bandsDiscard').disabled = !d;
  setMsg('bandsMsg', d ? 'Unsaved changes' : 'Applied bands are kept by the modem and survive reboots.');
}

function renderBands() {
  for (const g of GROUPS) {
    const active = new Set(parseBands(Bands.edit[g.key]));
    const configured = new Set(parseBands(Bands.configured[g.key]));
    const eff = Bands.effective[g.key] != null ? new Set(parseBands(Bands.effective[g.key])) : null;
    const hw = g.kind === 'lte' ? Bands.hwLte : Bands.hwNr;
    // the carriers the modem is on right now, in the group that applies
    const live = g.kind === 'lte' ? Bands.live.lte
      : (g.label === 'NSA') === /NSA/.test(Bands.live.rat) ? Bands.live.nr : new Set();
    const all = Array.from(new Set([...universe(g.kind), ...active])).sort((a, b) => a - b);
    $('#' + g.el).innerHTML = all.map(b => {
      let cls = 'chip', title = 'Off - click to enable';
      if (active.has(b)) {
        const unusable = configured.has(b) && eff && !eff.has(b);
        const unsupported = hw && !hw.includes(b);
        if (unusable || unsupported) { cls += ' warn'; title = "Enabled, but this modem can't use it - click to remove"; }
        else { cls += ' on'; title = 'On - click to disable'; }
      }
      if (live.has(b)) { cls += ' live'; title += ' (in use right now)'; }
      return `<button type="button" class="${cls}" data-g="${g.key}" data-b="${b}" title="${title}" aria-pressed="${active.has(b)}">${g.prefix}${b}</button>`;
    }).join('');
  }
  const current = REGIONS.find(r =>
    Bands.edit.nr5g_band === joinBands(presetFor(r, 'nr')) &&
    Bands.edit.nsa_nr5g_band === joinBands(presetFor(r, 'nr')) &&
    Bands.edit.lte_band === joinBands(presetFor(r, 'lte')));
  $$('#presets button').forEach(b => b.classList.toggle('active', b.dataset.region === current));
  renderBandsNote();
}

function renderBandsNote() {
  const note = $('#bandsNote');
  if (!Bands.hwNr) {
    note.textContent = "Modem model not recognised - showing every regional band. Any the modem can't use are marked after Apply.";
    return;
  }
  note.textContent = `This modem supports LTE ${Bands.hwLte.map(b => 'B' + b).join(' ')} and 5G ${Bands.hwNr.map(b => 'n' + b).join(' ')}. Presets only pick from these.`;
}

$('#v-cellular').addEventListener('click', e => {
  const chip = e.target.closest('.chip[data-g]');
  if (chip) {
    const set = new Set(parseBands(Bands.edit[chip.dataset.g]));
    const b = Number(chip.dataset.b);
    if (set.has(b)) set.delete(b); else set.add(b);
    Bands.edit[chip.dataset.g] = joinBands(Array.from(set));
    setBandsDirty(true);
    renderBands();
    return;
  }
  const preset = e.target.closest('#presets button');
  if (preset) {
    const r = preset.dataset.region;
    Bands.edit.nr5g_band = joinBands(presetFor(r, 'nr'));
    Bands.edit.nsa_nr5g_band = Bands.edit.nr5g_band;
    Bands.edit.lte_band = joinBands(presetFor(r, 'lte'));
    setBandsDirty(true);
    renderBands();
  }
});

function syncFromModem(m) {
  Bands.configured = { nr5g_band: m.nr5g_band || '', nsa_nr5g_band: m.nsa_nr5g_band || '', lte_band: m.lte_band || '' };
  Bands.effective = {};
  GROUPS.forEach(g => { if (m['effective_' + g.key] != null) Bands.effective[g.key] = m['effective_' + g.key]; });
  Bands.hwNr = m.hw_nr5g_band ? parseBands(m.hw_nr5g_band) : null;
  Bands.hwLte = m.hw_lte_band ? parseBands(m.hw_lte_band) : null;
  Bands.model = m.modem_model || '';
  setText('modemModel', Bands.model);
  // a refresh never overwrites an edit that hasn't been applied yet
  if (!Bands.dirty) Bands.edit = Object.assign({}, Bands.configured);
  renderBands();
}

$('#bandsDiscard').addEventListener('click', () => {
  Bands.edit = Object.assign({}, Bands.configured);
  setBandsDirty(false);
  renderBands();
});

$('#bandsApply').addEventListener('click', e => withBusy(e.currentTarget, async () => {
  setMsg('bandsMsg', 'Applying - the modem may reconnect, this can take up to a minute…');
  let m;
  try {
    m = await api('/api/bands', Bands.edit);
  } catch (err) {
    setMsg('bandsMsg', err.message, 'err');
    throw err;
  }
  setBandsDirty(false);
  syncFromModem(m);
  const unusable = GROUPS.map(g => {
    if (m['effective_' + g.key] == null) return '';
    const eff = new Set(parseBands(m['effective_' + g.key]));
    const bad = parseBands(m[g.key]).filter(b => !eff.has(b));
    return bad.length ? `${g.label} ${bad.map(b => g.prefix + b).join(' ')}` : '';
  }).filter(Boolean);
  if (unusable.length) setMsg('bandsMsg', "Applied, but the modem can't use: " + unusable.join('; '), 'err');
  else { setMsg('bandsMsg', 'Applied', 'ok'); toast('Bands applied'); }
  refresh('cellular');
}).catch(() => {}));

// --------------------------------------------------------------- 5G mode ---

const MODE_INFO = {
  0: ['SA + NSA', 'good', 'The modem uses standalone or non-standalone 5G, whichever the network offers. Best for most people.'],
  1: ['NSA only', 'accent', '5G only on top of an LTE anchor - how most operators run 5G today. Rules out a weak SA cell.'],
  2: ['SA only', 'accent', 'Standalone 5G only. Lower latency where it exists; without SA coverage you fall back to LTE.'],
  3: ['LTE only', 'warn', '5G is switched off. Sometimes steadier on the edge of 5G coverage.'],
};
let modeBusy = false;

function renderMode(mode) {
  if (mode == null || modeBusy) return;
  $$('#modeSeg input').forEach(i => { i.checked = i.value === String(mode); });
  const info = MODE_INFO[mode] || [String(mode), '', ''];
  const badge = $('#modeBadge');
  badge.className = 'pill ' + info[1];
  badge.textContent = info[0];
  setText('modeHelp', info[2]);
}

$('#modeSeg').addEventListener('change', async e => {
  const mode = parseInt(e.target.value, 10);
  const seg = $('#modeSeg');
  modeBusy = true;
  seg.classList.add('busy');
  setText('modeHelp', 'Switching - the connection may drop for a few seconds…');
  try {
    await api('/api/sa', { mode });
    toast(`5G mode: ${MODE_INFO[mode][0]}`);
  } catch (err) {
    toast(err.message, 'err');
  } finally {
    modeBusy = false;
    seg.classList.remove('busy');
    refresh('status');
    setTimeout(() => refresh('cellular'), 4000);
  }
});

on('status', (s, err) => {
  if (!s) return;
  const m = s.modem || {};
  if (m._error) {
    $('#bandsNote').textContent = "Couldn't read the modem: " + cleanError(m._error);
    return;
  }
  renderMode(m.nr5g_disable_mode);
  syncFromModem(m);
});

// ------------------------------------------------------------ connection ---

on('cellular', c => {
  if (!c) return;
  const reg = $('#celRegPill');
  reg.className = 'pill ' + (c.registered ? (c.roaming ? 'warn' : 'good') : 'bad');
  reg.innerHTML = `<span class="dot"></span>${c.registered ? (c.roaming ? 'Roaming' : 'Registered') : 'Not registered'}`;

  let sim = c.sim_status || '—';
  if (c.sim_locked) sim += ' · PIN on' + (c.sim_pin_left != null && c.sim_pin_left !== 3 ? ` (${c.sim_pin_left} tries left)` : '');
  else if (c.sim_status === 'Ready') sim += ' · PIN off';

  const rows = [
    ['Operator', c.operator],
    ['Network', c.network_type],
    ['Carriers', c.band ? c.band.replace(/\+/g, ' + ') : ''],
    ['LTE cell', [c.band_primary, c.pci ? 'PCI ' + c.pci : '', c.earfcn ? 'EARFCN ' + c.earfcn : ''].filter(Boolean).join(' · ')],
    ['5G cell', [c.band_5g, c.pci_5g ? 'PCI ' + c.pci_5g : '', c.nr_arfcn ? 'ARFCN ' + c.nr_arfcn : ''].filter(Boolean).join(' · ')],
    ['APN', c.apn],
    ['SIM', sim],
  ];
  $('#celInfo').innerHTML = kvRows(rows);

  Bands.live = {
    rat: c.network_type || '',
    lte: new Set((c.band || '').match(/B(\d+)/g)?.map(x => Number(x.slice(1))) || []),
    nr: new Set((c.band || '').match(/n(\d+)/g)?.map(x => Number(x.slice(1))) || []),
  };
  if (Bands.model) renderBands();
});

// ---------------------------------------------------- data, roaming, PIN ---

on('netcfg', d => {
  if (!d) return;
  $('#mdData').checked = String(d.networkdata) === '1';
  $('#mdRoam').checked = String(d.networkroam) === '1';
  $('#mdData').disabled = $('#mdRoam').disabled = false;
});

async function saveNetCfg(e) {
  const cur = Sources.netcfg.data || {};
  const data = $('#mdData').checked, roam = $('#mdRoam').checked;
  if (e.target.id === 'mdData' && !data && !confirm('Turn mobile data off? The router loses its internet connection until you turn it back on here.')) {
    e.target.checked = true;
    return;
  }
  $('#mdData').disabled = $('#mdRoam').disabled = true;
  try {
    await stock('netcfg_set', { networkdata: data ? 1 : 0, networkroam: roam ? 1 : 0, networktype: cur.networktype || 'auto' }, ['netcfg']);
    toast(e.target.id === 'mdData' ? `Mobile data ${data ? 'on' : 'off'}` : `Roaming ${roam ? 'allowed' : 'off'}`);
    setTimeout(() => refresh('cellular'), 5000);
  } catch (err) {
    toast(err.message, 'err');
    e.target.checked = !e.target.checked;
  } finally {
    $('#mdData').disabled = $('#mdRoam').disabled = false;
  }
}
$('#mdData').addEventListener('change', saveNetCfg);
$('#mdRoam').addEventListener('change', saveNetCfg);

on('pin', d => {
  if (!d) return;
  setText('pinRetry', `${d.pinretry} PIN tries left · PUK ${d.pukretry}`);
});
on('cellular', c => {
  if (!c) return;
  $('#pinLock').checked = !!c.sim_locked;
  $('#pinLock').disabled = c.sim_status !== 'Ready';
});

// ------------------------------------------------------------ SIM lock ---
// Whether the SIM wants its PIN/PUK comes from the stock GetSIMStatus - the
// same check the stock PIN dialog uses. It answers while the SIM is locked,
// when the cellular read (daemon status + AT) may not, so the prompt shows
// up on every page as soon as the panel opens after a reboot.

function simLockState() {
  const s = Sources.simstatus.data, c = Sources.cellular.data;
  if (s && s.status != null) {
    return { pin: s.status === 2, puk: s.status === 3, pinLeft: s.pinretry, pukLeft: s.pukretry, autopin: String(s.autopin) === '1' };
  }
  if (c) return { pin: c.sim_status === 'PIN required', puk: c.sim_status === 'PUK required', pinLeft: c.sim_pin_left, pukLeft: c.sim_puk_left };
  return null;
}

// Unlocks with the stock PIN dialog's own field names (pincode/autopin,
// pukcode/newpin). When the SIM used up a try the code was wrong, and the
// error says how many tries are left.
async function unlockSim({ pin, puk, remember }) {
  const after = ['simstatus', 'pin', 'autopin', 'cellular', 'status'];
  const before = Sources.simstatus.data || {};
  try {
    if (puk) await stock('puk_verify', { pukcode: puk, newpin: pin }, after);
    else await stock('pin_verify', { pincode: pin, autopin: remember ? 1 : 0 }, after);
  } catch (err) {
    await refresh('simstatus');
    const s = Sources.simstatus.data;
    // Only call it a wrong code when the SIM actually used up a try.
    const used = s && (puk ? s.pukretry < before.pukretry : s.pinretry < before.pinretry || s.status === 3);
    if (!used) throw err;
    if (puk) throw new Error(s.pukretry > 0 ? `Wrong PUK - ${s.pukretry} tries left` : 'Wrong PUK - the SIM is now blocked for good');
    throw new Error(s.pinretry > 0 ? `Wrong PIN - ${s.pinretry} ${s.pinretry === 1 ? 'try' : 'tries'} left` : 'Wrong PIN - the SIM now needs its PUK');
  }
}

function lockText(st) {
  return st.puk
    ? `Too many wrong PINs - unblock the SIM with its PUK and set a new PIN.${st.pukLeft != null ? ` ${st.pukLeft} PUK tries left.` : ''}`
    : `The SIM is locked - enter the PIN to bring cellular back up.${st.pinLeft != null && st.pinLeft < 3 ? ` Only ${st.pinLeft} ${st.pinLeft === 1 ? 'try' : 'tries'} left before the PUK is needed.` : ''}`;
}

const SimLock = { dismissed: null };

function renderSimLock() {
  const st = simLockState();
  const locked = !!st && (st.pin || st.puk);
  $('#simLockBanner').hidden = !locked;
  $('#pinUnlockBox').hidden = !locked;
  const dlg = $('#pinDialog');
  if (!locked) {
    SimLock.dismissed = null;
    if (dlg.open) dlg.close();
    return;
  }
  const key = st.puk ? 'puk' : 'pin';
  setText('simLockText', st.puk ? 'SIM is blocked - unblock it with the PUK. '
    : `SIM is locked. ${st.pinLeft != null ? st.pinLeft + ' PIN tries left. ' : ''}`);
  // Cellular tab form
  $('#pinPukField').hidden = !st.puk;
  $('#pinPukField').querySelector('input').required = st.puk;
  $('#pinUnlockForm').pin.placeholder = st.puk ? 'new PIN (4-8 digits)' : '4-8 digits';
  setText('pinUnlockMsg', lockText(st));
  // Prompt on every page, once per lock state unless dismissed
  const f = $('#pinDialogForm');
  setText('pinDialogTitle', st.puk ? 'SIM is blocked' : 'SIM is locked');
  setText('pinDialogText', lockText(st));
  $('#pinDialogPukField').hidden = !st.puk;
  f.puk.required = st.puk;
  setText('pinDialogPinLabel', st.puk ? 'New PIN' : 'PIN');
  $('#pinDialogRememberRow').hidden = st.puk;
  if (!dlg.open && SimLock.dismissed !== key) {
    f.reset();
    f.remember.checked = st.autopin !== false;
    $('#pinDialogMsg').textContent = '';
    dlg.showModal();
    f.elements[st.puk ? 'puk' : 'pin'].focus();
  }
}
on('simstatus', renderSimLock);
on('cellular', renderSimLock);

$('#simLockLink').addEventListener('click', e => {
  e.preventDefault();
  SimLock.dismissed = null;
  renderSimLock();
});
$('#pinDialogLater').addEventListener('click', () => {
  const st = simLockState();
  SimLock.dismissed = st && st.puk ? 'puk' : 'pin';
  $('#pinDialog').close();
});
$('#pinDialog').addEventListener('cancel', () => {
  const st = simLockState();
  SimLock.dismissed = st && st.puk ? 'puk' : 'pin';
});

$('#pinDialogForm').addEventListener('submit', async e => {
  e.preventDefault();
  const f = e.target;
  const withPuk = !$('#pinDialogPukField').hidden;
  const pin = f.pin.value.trim(), puk = f.puk.value.trim();
  if (withPuk && !/^\d{8}$/.test(puk)) return void ($('#pinDialogMsg').textContent = 'A PUK is 8 digits');
  if (!/^\d{4,8}$/.test(pin)) return void ($('#pinDialogMsg').textContent = 'A PIN is 4 to 8 digits');
  $('#pinDialogMsg').textContent = '';
  try {
    await withBusy($('#pinDialogBtn'), () => unlockSim({ pin, puk: withPuk ? puk : '', remember: f.remember.checked }));
    $('#pinDialog').close();
    toast(withPuk ? 'SIM unblocked - new PIN set' : 'SIM unlocked');
  } catch (err) {
    f.pin.value = '';
    $('#pinDialogMsg').textContent = err.message;
  }
});

$('#pinUnlockForm').addEventListener('submit', async e => {
  e.preventDefault();
  const f = e.target;
  const pin = f.pin.value.trim();
  if (!/^\d{4,8}$/.test(pin)) return toast('A PIN is 4 to 8 digits', 'err');
  const puk = f.puk.value.trim();
  const btn = $('#pinUnlockBtn');
  const withPuk = !$('#pinPukField').hidden;
  if (withPuk && !/^\d{8}$/.test(puk)) return toast('A PUK is 8 digits', 'err');
  const remember = Sources.autopin.data ? String(Sources.autopin.data.autopin) === '1' : true;
  withBusy(btn, async () => {
    await unlockSim({ pin, puk: withPuk ? puk : '', remember });
    toast(withPuk ? 'SIM unblocked - new PIN set' : 'SIM unlocked');
    f.reset();
  }).catch(() => {});
});

on('autopin', d => {
  if (!d) return;
  $('#autoPin').checked = String(d.autopin) === '1';
  $('#autoPin').disabled = false;
});

function askPin(text) {
  const pin = prompt(text);
  if (pin == null) return null;
  if (!/^\d{4,8}$/.test(pin)) { toast('A PIN is 4 to 8 digits', 'err'); return null; }
  return pin;
}

$('#pinLock').addEventListener('change', async e => {
  const want = e.target.checked;
  const tries = Sources.pin.data?.pinretry;
  const pin = askPin(`Enter the SIM PIN to turn the PIN request ${want ? 'on' : 'off'}.${tries != null ? `\n${tries} tries left - after that the SIM needs its PUK.` : ''}`);
  if (!pin) { e.target.checked = !want; return; }
  e.target.disabled = true;
  try {
    await stock('pin_lock', { pinswitch: want ? 1 : 0, pincode: pin }, ['pin', 'cellular']);
    toast(want ? 'The SIM now asks for its PIN' : 'PIN request off');
  } catch (err) {
    toast(err.message, 'err');
    e.target.checked = !want;
    refresh('pin');
  } finally {
    e.target.disabled = false;
  }
});

$('#autoPin').addEventListener('change', async e => {
  const want = e.target.checked;
  e.target.disabled = true;
  try {
    await stock('autopin_set', { autopin: want ? 1 : 0 }, ['autopin']);
    toast(want ? 'The router unlocks the SIM by itself' : "The router won't store the PIN");
  } catch (err) {
    toast(err.message, 'err');
    e.target.checked = !want;
  } finally {
    e.target.disabled = false;
  }
});

$('#pinChangeBtn').addEventListener('click', async e => {
  const oldpin = askPin('Current SIM PIN:');
  if (!oldpin) return;
  const newpin = askPin('New PIN (4-8 digits):');
  if (!newpin) return;
  if (askPin('New PIN again:') !== newpin) return toast("The new PINs don't match", 'err');
  withBusy(e.currentTarget, async () => {
    await stock('pin_change', { oldpin, newpin }, ['pin']);
    toast('SIM PIN changed');
  }).catch(() => refresh('pin'));
});

// ------------------------------------------------------------------ APN ---

on('apn', (d, err) => {
  const box = $('#apnList');
  if (!d) {
    if (err) box.innerHTML = `<div class="empty">${esc(err.message)}</div>`;
    return;
  }
  const list = arr(d.apnlist);
  box.innerHTML = list.length ? list.map(a => {
    const active = a.id === d.curid;
    return `<div class="dev">
      <div class="avatar ${active ? 'online' : ''}">${icon(active ? 'check' : 'globe')}</div>
      <div class="main" style="cursor:default"><div class="name">${esc(a.file || a.apn)}</div>
        <div class="meta">${esc([a.apn, a.pdp, a.user ? 'user ' + a.user : ''].filter(Boolean).join(' · '))}</div></div>
      <div class="row-actions">
        ${active ? '<span class="pill good">In use</span>' : `<button class="small" data-apn-use="${esc(a.id)}">Use</button>`}
        ${a.noedit === '1' ? '' : `<button class="ghost small" data-apn-edit="${esc(a.id)}">Edit</button>
          ${active ? '' : `<button class="ghost small" data-apn-del="${esc(a.id)}" aria-label="Delete">${icon('trash')}</button>`}`}
      </div></div>`;
  }).join('') : '<div class="empty">No access points.</div>';
});

function apnById(id) { return arr(Sources.apn.data?.apnlist).find(a => a.id === id); }

function openApnForm(a) {
  const f = $('#apnForm');
  f.hidden = false;
  for (const k of ['file', 'apn', 'user', 'passwd', 'id']) f[k].value = a ? a[k] || '' : '';
  f.pdp.value = a?.pdp || 'IPv4 & IPv6';
  // stock values are None/PAP/CHAP; older entries may say NONE
  f.encryption.value = /^none$/i.test(a?.encryption || '') || !a?.encryption ? 'None' : a.encryption;
  f.file.focus();
}
$('#apnAddBtn').addEventListener('click', () => openApnForm(null));
$('#apnCancel').addEventListener('click', () => { $('#apnForm').hidden = true; });
$('#apnForm').addEventListener('submit', e => {
  e.preventDefault();
  const f = e.target;
  const body = Object.fromEntries(['id', 'file', 'apn', 'user', 'passwd', 'pdp', 'encryption'].map(k => [k, f[k].value.trim()]));
  withBusy($('button[type=submit]', f), async () => {
    await stock('apn_save', body, ['apn']);
    f.hidden = true;
    toast('Access point saved');
  }).catch(() => {});
});

$('#apnList').addEventListener('click', e => {
  const use = e.target.closest('[data-apn-use]');
  const edit = e.target.closest('[data-apn-edit]');
  const del = e.target.closest('[data-apn-del]');
  if (edit) return openApnForm(apnById(edit.dataset.apnEdit));
  if (use) {
    const a = apnById(use.dataset.apnUse);
    if (!confirm(`Connect with "${a.file || a.apn}"? The mobile connection drops for a moment.`)) return;
    withBusy(use, async () => {
      await stock('apn_apply', { id: a.id }, ['apn']);
      toast('Reconnecting with the new access point');
      setTimeout(() => refresh('cellular'), 8000);
    }).catch(() => {});
  }
  if (del) {
    const a = apnById(del.dataset.apnDel);
    if (!confirm(`Delete the access point "${a.file || a.apn}"?`)) return;
    withBusy(del, async () => {
      await stock('apn_delete', { id: a.id }, ['apn']);
      toast('Access point deleted');
    }).catch(() => {});
  }
});

// ------------------------------------------------------------------- SMSC ---

async function readSMSC() {
  try {
    const d = await api('/api/smsc');
    setText('smscCurrent', d.smsc || '—');
  } catch (err) {
    setMsg('smscMsg', err.message, 'err');
  }
}
$('#smscApplyBtn').addEventListener('click', async e => {
  const num = ($('#smscNew').value || '').replace(/[\s()-]/g, '');
  if (!/^\+?\d{3,20}$/.test(num)) return setMsg('smscMsg', 'Enter a phone number, like +491710760000', 'err');
  try {
    await withBusy(e.currentTarget, async () => {
      const d = await api('/api/smsc', { smsc: num });
      setText('smscCurrent', d.smsc || num);
      $('#smscNew').value = '';
      setMsg('smscMsg', 'Saved', 'ok');
    });
  } catch (err) {
    setMsg('smscMsg', err.message, 'err');
  }
});
window.addEventListener('hashchange', () => {
  if (location.hash === '#cellular' && $('#smscCurrent').textContent === '—') readSMSC();
});
if (location.hash === '#cellular') setTimeout(readSMSC, 800);

// ---------------------------------------------------------- own number ---

on('cellular', c => { if (c) setText('simNumCurrent', c.sim_number || 'not stored'); });

$('#simNumBtn').addEventListener('click', async e => {
  const num = ($('#simNumNew').value || '').replace(/[\s()-]/g, '');
  if (!/^\+\d{6,20}$/.test(num)) return setMsg('simNumMsg', 'International format with +, like +381601234567', 'err');
  if (!confirm(`Write ${num} to the SIM as its own number?`)) return;
  try {
    await withBusy(e.currentTarget, async () => {
      const d = await api('/api/sim-number', { number: num });
      setText('simNumCurrent', d.number || num);
      $('#simNumNew').value = '';
      setMsg('simNumMsg', 'Written to the SIM', 'ok');
      refresh('cellular');
    });
  } catch (err) {
    setMsg('simNumMsg', err.message, 'err');
  }
});

// ----------------------------------------------------------- cell lock ---

const Lock = { serving: {} };

function lockLteRow(c) {
  const row = document.createElement('div');
  row.className = 'lock-row';
  row.innerHTML = `<input type="number" class="lk-earfcn" min="1" placeholder="EARFCN" aria-label="EARFCN" value="${c ? c.earfcn : ''}">
    <input type="number" class="lk-pci" min="0" max="503" placeholder="PCI" aria-label="PCI" value="${c ? c.pci : ''}">
    <button class="ghost small" type="button" aria-label="Remove">${icon('trash')}</button>`;
  $('button', row).addEventListener('click', () => row.remove());
  $('#lockLte').appendChild(row);
}

function renderLock(d) {
  Lock.serving = { lte: d.serving_lte, nr: d.serving_nr };
  const lte = arr(d.lte), nr = d.nr;
  const pill = $('#lockPill');
  const n = lte.length + (nr ? 1 : 0);
  pill.className = 'pill ' + (n ? 'warn' : 'good');
  pill.textContent = n ? `Locked · ${[lte.length ? lte.length + ' LTE' : '', nr ? '5G' : ''].filter(Boolean).join(' + ')}` : 'Not locked';
  $('#lockLte').innerHTML = '';
  lte.forEach(lockLteRow);
  $('#lockNrPci').value = nr ? nr.pci : '';
  $('#lockNrArfcn').value = nr ? nr.arfcn : '';
  $('#lockNrScs').value = nr ? String(nr.scs) : '15';
  $('#lockNrBand').value = nr ? nr.band : '';
  $('#lockPersist').checked = !!d.persist;
  $('#lockLteCur').disabled = !d.serving_lte;
  $('#lockNrCur').disabled = !d.serving_nr;
  const cur = [d.serving_lte && `LTE ${d.serving_lte.earfcn}/${d.serving_lte.pci}`,
    d.serving_nr && `5G n${d.serving_nr.band} ${d.serving_nr.arfcn}/${d.serving_nr.pci}`].filter(Boolean);
  setMsg('lockMsg', cur.length ? 'On now: ' + cur.join(' · ') : 'No serving cell reported right now.');
}

async function readLock() {
  try {
    renderLock(await api('/api/cell-lock'));
  } catch (err) {
    setMsg('lockMsg', err.message, 'err');
  }
}

$('#lockLteAdd').addEventListener('click', () => {
  if ($$('#lockLte .lock-row').length >= 10) return toast('The modem takes at most 10 LTE cells', 'err');
  lockLteRow(null);
});
$('#lockLteCur').addEventListener('click', () => {
  const s = Lock.serving.lte;
  if (!s) return;
  if ($$('#lockLte .lock-row').some(r => $('.lk-earfcn', r).value == s.earfcn && $('.lk-pci', r).value == s.pci)) return;
  lockLteRow(s);
});
$('#lockNrCur').addEventListener('click', () => {
  const s = Lock.serving.nr;
  if (!s) return;
  $('#lockNrPci').value = s.pci;
  $('#lockNrArfcn').value = s.arfcn;
  $('#lockNrScs').value = String(s.scs);
  $('#lockNrBand').value = s.band;
});
$('#lockNrClear').addEventListener('click', () => {
  ['#lockNrPci', '#lockNrArfcn', '#lockNrBand'].forEach(id => { $(id).value = ''; });
});

async function applyLock(btn, body, what) {
  setMsg('lockMsg', 'Applying - the modem may reconnect…');
  try {
    await withBusy(btn, async () => {
      renderLock(await api('/api/cell-lock', body));
      toast(what);
      setTimeout(() => refresh('cellular'), 5000);
    });
  } catch (err) {
    setMsg('lockMsg', err.message, 'err');
  }
}

$('#lockApply').addEventListener('click', e => {
  const rows = $$('#lockLte .lock-row').map(r => [$('.lk-earfcn', r).value, $('.lk-pci', r).value])
    .filter(([e, p]) => e !== '' || p !== '');
  if (rows.some(([e, p]) => e === '' || p === '')) return setMsg('lockMsg', 'Every LTE cell needs an EARFCN and a PCI', 'err');
  const lte = rows.map(([e, p]) => ({ earfcn: +e, pci: +p }));
  const f = ['#lockNrPci', '#lockNrArfcn', '#lockNrBand'].map(id => $(id).value);
  let nr = null;
  if (f.some(Boolean)) {
    if (!f.every(Boolean)) return setMsg('lockMsg', 'The 5G lock needs PCI, ARFCN and band', 'err');
    nr = { pci: +f[0], arfcn: +f[1], scs: +$('#lockNrScs').value, band: +f[2] };
  }
  if (!lte.length && !nr) return setMsg('lockMsg', 'Add a cell first - or use Unlock all', 'err');
  if (!confirm('Lock the modem to these cells? If none of them is reachable you have no mobile service until you unlock.')) return;
  applyLock(e.currentTarget, { lte, nr, persist: $('#lockPersist').checked }, 'Cell lock applied');
});
$('#lockClearAll').addEventListener('click', e => {
  applyLock(e.currentTarget, { lte: [], nr: null, persist: false }, 'Cell lock removed');
});

window.addEventListener('hashchange', () => {
  if (location.hash === '#cellular' && $('#lockPill').textContent === '—') readLock();
});
if (location.hash === '#cellular') setTimeout(readLock, 1200);
