'use strict';
// Messages: the SIM's SMS inbox, conversations, sending and deleting,
// through the stock SMS actions.

Views.messages = { sources: ['sms', 'contacts'], show() {
  const to = viewQuery.get('to');
  if (to) { history.replaceState(null, '', '#messages'); composeTo(to); }
  else if (Sms.peer) loadThread();
} };

// SIM contacts give senders a name; the list re-renders when they arrive.
function contactName(num) {
  const n = String(num).replace(/[\s()-]/g, '');
  const c = arr(Sources.contacts.data?.contacts).find(x => x.number === n || (n.length > 6 && x.number.endsWith(n.slice(-9))));
  return c ? c.name : null;
}
on('contacts', () => { if (Sources.sms.data) publish('sms', Sources.sms.data); });

// stock message state bits: 8 received, 4 sent, 2 draft, 1 read. Opening a
// conversation (get_dialog_msg) marks it read, as on the stock page.
const isUnread = m => (m.state & 8) && !(m.state & 1);
function smsHint(t) { $('#smsLen').textContent = t || ''; }

const Sms = { peer: null, peerRaw: null, lastId: null, composing: false };

// Senders like "Telekom" can't be replied to - only real numbers can.
const canReply = p => /^\+?\d{3,20}$/.test(String(p).replace(/[\s()-]/g, ''));
// Some SIM sender names carry a byte that isn't valid text; show it without.
const peerName = p => contactName(p) || String(p).replace(/\uFFFD/g, '').trim() || 'Unknown sender';

function fmtSmsDate(m) {
  if (!m.timestamp) return m.date || '';
  const d = new Date(m.timestamp * 1000), now = new Date();
  return d.toDateString() === now.toDateString()
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { day: 'numeric', month: 'short' });
}

on('sms', (d, err) => {
  const box = $('#smsList');
  if (!d) {
    if (err) box.innerHTML = `<div class="empty">${esc(err.message)}</div>`;
    return;
  }
  const list = arr(d.msgbox);
  const unread = list.filter(isUnread).length;
  setText('smsCount', unread ? `${unread} unread` : `${list.length} conversation${list.length === 1 ? '' : 's'}`);
  const badge = $('#unreadCount');
  badge.hidden = !unread;
  badge.textContent = unread;
  box.innerHTML = list.length ? list.map(m => `
    <div class="sms-item ${isUnread(m) ? 'unread' : ''} ${m.contact_phone === Sms.peer ? 'active' : ''}" data-peer="${esc(m.contact_phone)}" data-raw="${esc(m.contact_phone_b64 || '')}" data-id="${m.msg_id}" tabindex="0">
      <div class="avatar ${isUnread(m) ? 'online' : ''}">${icon('msg')}</div>
      <div class="main"><div class="name">${esc(peerName(m.contact_phone))}<span>${esc(fmtSmsDate(m))}</span></div>
        <div class="meta">${m.state & 2 ? '<span class="q-fair">Draft: </span>' : ''}${esc(m.content)}</div></div>
      ${isUnread(m) ? '<span class="udot"></span>' : ''}
    </div>`).join('') : '<div class="empty">No messages on the SIM.</div>';
});

function openThread(peer, id, raw) {
  Sms.peer = peer;
  Sms.peerRaw = raw || null;
  Sms.lastId = id;
  Sms.composing = false;
  $('#v-messages').classList.add('in-thread');
  $('#smsToRow').hidden = true;
  $('#smsDelete').hidden = false;
  const reply = canReply(peer);
  $('#smsForm').hidden = !reply;
  smsHint(reply ? '' : "This sender doesn't accept replies.");
  setText('smsPeer', peerName(peer));
  $('#smsPeer').title = String(peer);
  $$('.sms-item').forEach(el => el.classList.toggle('active', el.dataset.peer === peer));
  loadThread();
}

async function loadThread() {
  if (!Sms.peer) return;
  const box = $('#smsThread');
  box.innerHTML = '<div class="empty">Loading…</div>';
  try {
    // msg_id is the row the stock code marks read, so it must be this
    // conversation's own: after starting a new one, take it from the inbox.
    if (!Sms.lastId) {
      await refresh('sms');
      const row = arr(Sources.sms.data?.msgbox).find(m => m.contact_phone === Sms.peer);
      if (!row) { box.innerHTML = '<div class="empty">No messages.</div>'; return; }
      Sms.lastId = row.msg_id;
    }
    const q = new URLSearchParams({ a: 'sms_thread', msg_id: Sms.lastId });
    if (Sms.peerRaw) q.set('phoneNum_b64', Sms.peerRaw); else q.set('phoneNum', Sms.peer);
    const d = await api('/api/stock?' + q);
    const msgs = arr(d.msgdialoglist).slice().sort((a, b) => a.timestamp - b.timestamp);
    box.innerHTML = msgs.map(m => `<div class="bubble ${m.state & 8 ? '' : 'out'}">${esc(m.content)}<small>${esc(m.date || '')}</small></div>`).join('')
      || '<div class="empty">No messages.</div>';
    box.scrollTop = box.scrollHeight;
    if (arr(Sources.sms.data?.msgbox).some(m => m.contact_phone === Sms.peer && isUnread(m))) {
      refresh('sms'); // reading the thread marked it read on the router
    }
  } catch (err) {
    box.innerHTML = `<div class="empty">${esc(err.message)}</div>`;
  }
}

$('#smsList').addEventListener('click', e => {
  const it = e.target.closest('.sms-item');
  if (it) openThread(it.dataset.peer, it.dataset.id, it.dataset.raw);
});
$('#smsList').addEventListener('keydown', e => {
  const it = e.target.closest('.sms-item');
  if (it && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); openThread(it.dataset.peer, it.dataset.id, it.dataset.raw); }
});

function composeTo(to) {
  const n = to.replace(/[\s()-]/g, '');
  const row = arr(Sources.sms.data?.msgbox).find(m => String(m.contact_phone).replace(/[\s()-]/g, '') === n);
  if (row) return openThread(row.contact_phone, row.msg_id, row.contact_phone_b64);
  $('#smsNewBtn').click();
  $('#smsTo').value = to;
  $('#smsText').focus();
}

$('#smsNewBtn').addEventListener('click', () => {
  Sms.peer = null;
  Sms.peerRaw = null;
  Sms.lastId = null;
  Sms.composing = true;
  $('#v-messages').classList.add('in-thread');
  setText('smsPeer', 'New message');
  $('#smsToRow').hidden = false;
  $('#smsDelete').hidden = true;
  $('#smsForm').hidden = false;
  $('#smsThread').innerHTML = '';
  $$('.sms-item').forEach(el => el.classList.remove('active'));
  $('#smsTo').value = '';
  smsHint('');
  $('#smsTo').focus();
});

$('#smsBack').addEventListener('click', () => { $('#v-messages').classList.remove('in-thread'); });

// GSM-7 fits 160 characters per part, anything else (Cyrillic, emoji) 70.
function smsParts(text) {
  const gsm = /^[\n\r A-Za-z0-9@£$¥èéùìòÇØøÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ!"#¤%&'()*+,\-./:;<=>?¡ÄÖÑÜ§¿äöñüà^{}\\[~\]|€]*$/.test(text);
  const one = gsm ? 160 : 70, multi = gsm ? 153 : 67;
  const n = text.length <= one ? 1 : Math.ceil(text.length / multi);
  return `${text.length} characters · ${n} SMS`;
}
$('#smsText').addEventListener('input', e => { smsHint(e.target.value ? smsParts(e.target.value) : ''); });

$('#smsForm').addEventListener('submit', e => {
  e.preventDefault();
  const text = $('#smsText').value.trim();
  const to = Sms.composing ? $('#smsTo').value.replace(/[\s()-]/g, '') : Sms.peer;
  if (!to) return toast('Who should get it?', 'err');
  if (Sms.composing && !/^\+?\d{3,20}$/.test(to)) return toast('That doesn\'t look like a phone number', 'err');
  if (!text) return;
  withBusy($('button[type=submit]', e.target), async () => {
    await stock('sms_send', { contact_phone: to, msgtext: text }, ['sms']);
    $('#smsText').value = '';
    smsHint('');
    toast('Sent');
    if (Sms.composing) {
      Sms.composing = false;
      Sms.peer = to;
      Sms.peerRaw = null;
      $('#smsToRow').hidden = true;
      $('#smsDelete').hidden = false;
      setText('smsPeer', to);
    }
    loadThread();
  }).catch(() => {});
});
$('#smsText').addEventListener('keydown', e => {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); $('#smsForm').requestSubmit(); }
});

$('#smsDelete').addEventListener('click', e => {
  if (!Sms.peer || !confirm(`Delete the whole conversation with ${peerName(Sms.peer)} from the SIM?`)) return;
  withBusy(e.currentTarget, async () => {
    await stock('sms_delete', Sms.peerRaw ? { phoneList_b64: Sms.peerRaw } : { phoneList: Sms.peer }, ['sms']);
    toast('Conversation deleted');
    Sms.peer = null;
    setText('smsPeer', 'Pick a conversation');
    $('#smsThread').innerHTML = '';
    $('#smsForm').hidden = true;
    $('#smsDelete').hidden = true;
    $('#v-messages').classList.remove('in-thread');
  }).catch(() => {});
});
