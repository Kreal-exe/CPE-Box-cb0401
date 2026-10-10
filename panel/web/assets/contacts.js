'use strict';
// Contacts: the SIM card's phonebook - list, search, add, edit, delete,
// and .vcf import/export, over the modem's AT phonebook commands.

Views.contacts = { sources: ['contacts'], show() { if (!Sources.contacts.data) refresh('contacts'); } };

const Pb = { list: [], total: 0, used: 0, current: null, filter: '', selecting: false, sel: new Set() };

// A stable colour per contact, from the name, so the avatars tell them apart.
function pbHue(name) {
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.codePointAt(0)) % 360;
  return `hsl(${h} 70% 60%)`;
}
const pbInitial = n => (Array.from(n.trim())[0] || '#').toUpperCase();

function pbMatch(c, q) {
  if (!q) return true;
  const digits = q.replace(/[\s()-]/g, '');
  return c.name.toLowerCase().includes(q) || (digits && c.number.replace(/[\s()-]/g, '').includes(digits));
}
function pbMark(s, q) {
  if (!q) return esc(s);
  const i = s.toLowerCase().indexOf(q);
  if (i < 0) return esc(s);
  return esc(s.slice(0, i)) + '<mark>' + esc(s.slice(i, i + q.length)) + '</mark>' + esc(s.slice(i + q.length));
}

function renderContacts() {
  const box = $('#pbList');
  const q = Pb.filter.trim().toLowerCase();
  const list = Pb.list.filter(c => pbMatch(c, q));
  setText('pbCount', Pb.list.length ? `${Pb.list.length} on the SIM` : '');
  const pct = Pb.total ? Pb.used / Pb.total * 100 : 0;
  const fill = $('#pbUsage');
  fill.style.width = pct + '%';
  fill.className = 'fill ' + (pct >= 95 ? 'q-poor' : pct >= 80 ? 'q-fair' : '');
  setText('pbUsageText', Pb.total ? `${Pb.used} / ${Pb.total} slots` : '');
  if (!list.length) {
    box.innerHTML = `<div class="empty">${Pb.list.length ? 'No contact matches.' : 'No contacts on the SIM yet.'}</div>`;
    return;
  }
  let html = '', group = '';
  for (const c of list) {
    const g = /\p{L}/u.test(pbInitial(c.name)) ? pbInitial(c.name) : '#';
    if (g !== group && !q) { group = g; html += `<div class="pb-group">${esc(g)}</div>`; }
    html += `<div class="pb-item ${Pb.current && Pb.current.index === c.index ? 'active' : ''} ${Pb.sel.has(c.index) ? 'checked' : ''}" data-i="${c.index}" tabindex="0">
      <label class="pb-check" aria-label="Select"><input type="checkbox" data-sel="${c.index}" ${Pb.sel.has(c.index) ? 'checked' : ''}><span></span></label>
      <div class="avatar" style="--hue:${pbHue(c.name)}">${esc(pbInitial(c.name))}</div>
      <div class="main"><div class="name">${pbMark(c.name, q)}</div><div class="meta">${pbMark(c.number, q)}</div></div></div>`;
  }
  box.innerHTML = html;
  renderSelBar(list);
}

// ------------------------------------------------------------ multi-select ---

function visibleContacts() {
  const q = Pb.filter.trim().toLowerCase();
  return Pb.list.filter(c => pbMatch(c, q));
}
function renderSelBar(visible) {
  visible = visible || visibleContacts();
  $('#pbSelBar').hidden = !Pb.selecting;
  $('#v-contacts').classList.toggle('selecting', Pb.selecting);
  $('#pbSelectBtn').textContent = Pb.selecting ? 'Done' : 'Select';
  const n = Pb.sel.size;
  setText('pbSelCount', n ? `${n} selected` : 'Select contacts to delete');
  $('#pbSelDelete').disabled = !n;
  $('#pbSelDelete').innerHTML = `${icon('trash')}Delete${n ? ' ' + n : ''}`;
  const all = $('#pbSelAll');
  all.checked = visible.length > 0 && visible.every(c => Pb.sel.has(c.index));
  all.indeterminate = !all.checked && visible.some(c => Pb.sel.has(c.index));
}
function setSelecting(on) {
  Pb.selecting = on;
  if (!on) Pb.sel.clear();
  else closeContact();
  renderContacts();
}
$('#pbSelectBtn').addEventListener('click', () => setSelecting(!Pb.selecting));
$('#pbSelCancel').addEventListener('click', () => setSelecting(false));
$('#pbSelAll').addEventListener('change', e => {
  const vis = visibleContacts();
  if (e.target.checked) vis.forEach(c => Pb.sel.add(c.index)); else vis.forEach(c => Pb.sel.delete(c.index));
  renderContacts();
});
$('#pbSelDelete').addEventListener('click', e => {
  const idx = Array.from(Pb.sel);
  if (!idx.length) return;
  const names = idx.map(i => Pb.list.find(c => c.index === i)?.name).filter(Boolean);
  const preview = names.slice(0, 5).join(', ') + (names.length > 5 ? ` and ${names.length - 5} more` : '');
  if (!confirm(`Delete ${idx.length} contact${idx.length === 1 ? '' : 's'} from the SIM?\n\n${preview}`)) return;
  withBusy(e.currentTarget, async () => {
    const r = await api('/api/contacts?a=delete', { indexes: idx });
    toast(r.failed ? `${r.deleted} deleted, ${r.failed} refused by the SIM` : `${r.deleted} contact${r.deleted === 1 ? '' : 's'} deleted`, r.failed ? 'err' : 'ok');
    setSelecting(false);
    refresh('contacts');
  }).catch(() => {});
});

on('contacts', (d, err) => {
  if (!d) {
    if (err) $('#pbList').innerHTML = `<div class="empty">${esc(err.message)}</div>`;
    return;
  }
  Pb.list = arr(d.contacts);
  Pb.used = d.used || Pb.list.length;
  Pb.total = d.total || 0;
  renderContacts();
});

$('#pbSearch').addEventListener('input', e => { Pb.filter = e.target.value; renderContacts(); });

// --------------------------------------------------------------- editor ---

function openContact(c) {
  Pb.current = c;
  const f = $('#pbForm');
  $('#pbEmpty').hidden = true;
  f.hidden = false;
  f.name.value = c ? c.name : '';
  f.number.value = c ? c.number : '';
  f.index.value = c ? c.index : 0;
  setText('pbTitle', c ? c.name : 'New contact');
  setText('pbSlot', c ? `SIM slot ${c.index}` : '');
  $('#pbDelete').hidden = !c;
  $('#pbSms').hidden = !c;
  if (c) $('#pbSms').href = '#messages?to=' + encodeURIComponent(c.number);
  setMsg('pbMsg', '');
  pbAvatarFrom(f.name.value);
  pbNameLen();
  $('#v-contacts').classList.add('in-edit');
  $$('.pb-item').forEach(el => el.classList.toggle('active', !!c && +el.dataset.i === c.index));
  if (!c) f.name.focus();
}
function closeContact() {
  Pb.current = null;
  $('#pbForm').hidden = true;
  $('#pbEmpty').hidden = false;
  setText('pbTitle', 'Pick a contact');
  setText('pbSlot', '');
  $('#pbDelete').hidden = true;
  $('#v-contacts').classList.remove('in-edit');
  $$('.pb-item').forEach(el => el.classList.remove('active'));
}
function pbAvatarFrom(name) {
  const a = $('#pbAvatar');
  a.textContent = pbInitial(name || '');
  a.style.setProperty('--hue', pbHue(name || ''));
}
function pbNameLen() {
  const n = Array.from($('#pbForm').name.value).length;
  setText('pbNameLen', n ? `${n} / 16` : '');
}

$('#pbList').addEventListener('click', e => {
  const it = e.target.closest('.pb-item');
  if (!it) return;
  const i = +it.dataset.i;
  if (Pb.selecting) {
    if (e.target.matches('input[data-sel]')) { e.target.checked ? Pb.sel.add(i) : Pb.sel.delete(i); }
    else { Pb.sel.has(i) ? Pb.sel.delete(i) : Pb.sel.add(i); }
    it.classList.toggle('checked', Pb.sel.has(i));
    $('input[data-sel]', it).checked = Pb.sel.has(i);
    renderSelBar();
    return;
  }
  openContact(Pb.list.find(c => c.index === i));
});
$('#pbList').addEventListener('keydown', e => {
  const it = e.target.closest('.pb-item');
  if (it && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); it.click(); }
});
$('#pbNewBtn').addEventListener('click', () => openContact(null));
$('#pbBack').addEventListener('click', closeContact);
$('#pbCancel').addEventListener('click', closeContact);
$('#pbForm').name.addEventListener('input', e => { pbAvatarFrom(e.target.value); pbNameLen(); setText('pbTitle', e.target.value || 'New contact'); });

$('#pbForm').addEventListener('submit', e => {
  e.preventDefault();
  const f = e.target;
  const name = f.name.value.trim(), number = f.number.value.replace(/[\s()-]/g, '');
  if (!name) return setMsg('pbMsg', 'The contact needs a name', 'err');
  if (Array.from(name).length > 16) return setMsg('pbMsg', 'SIM names are at most 16 characters', 'err');
  if (!/^\+?[0-9*#]{1,40}$/.test(number)) return setMsg('pbMsg', 'Enter a phone number, like +49 170 1234567', 'err');
  const dup = Pb.list.find(c => c.number === number && c.index !== +f.index.value);
  if (dup && !confirm(`${dup.name} already has this number. Save anyway?`)) return;
  withBusy($('#pbSave'), async () => {
    const c = await api('/api/contacts?a=save', { index: +f.index.value, name, number });
    toast(f.index.value === '0' ? 'Contact added to the SIM' : 'Contact saved');
    await refresh('contacts');
    openContact(Pb.list.find(x => x.index === c.index) || c);
  }).catch(err => setMsg('pbMsg', err.message, 'err'));
});

$('#pbDelete').addEventListener('click', e => {
  const c = Pb.current;
  if (!c || !confirm(`Delete ${c.name} from the SIM?`)) return;
  withBusy(e.currentTarget, async () => {
    await api('/api/contacts?a=delete', { index: c.index });
    toast('Contact deleted');
    closeContact();
    refresh('contacts');
  }).catch(() => {});
});

// ------------------------------------------------------------- import ---

$('#pbImportBtn').addEventListener('click', () => $('#pbFile').click());
$('#pbFile').addEventListener('change', async e => {
  const file = e.target.files[0];
  e.target.value = '';
  if (!file) return;
  const vcf = await file.text();
  const n = (vcf.match(/BEGIN:VCARD/gi) || []).length;
  if (!n) return toast("That doesn't look like a .vcf file", 'err');
  const free = Pb.total ? Pb.total - Pb.used : null;
  if (!confirm(`Import ${n} contact${n === 1 ? '' : 's'} from ${file.name} to the SIM?${free != null ? `\n\n${free} free slots. Contacts whose number is already on the SIM are skipped; names longer than 16 characters are shortened.` : ''}`)) return;
  withBusy($('#pbImportBtn'), async () => {
    const r = await api('/api/contacts?a=import', { vcf });
    const parts = [`${r.added} added`];
    if (r.skipped) parts.push(`${r.skipped} already there`);
    if (r.failed) parts.push(`${r.failed} refused by the SIM`);
    if (r.left) parts.push(`${r.left} didn't fit`);
    toast(parts.join(' · '), r.failed || r.left ? 'err' : 'ok');
    refresh('contacts');
  }).catch(() => {});
});
