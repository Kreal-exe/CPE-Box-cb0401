'use strict';
// System (health, lights, reboot, access, SSH, password, firmware) and
// Console (raw commands).

Views.system = { sources: ['health', 'leds', 'info', 'ssh', 'rname'] };

on('rname', d => {
  if (d && document.activeElement !== $('#routerName')) $('#routerName').value = d.name || '';
});
$('#nameForm').addEventListener('submit', e => {
  e.preventDefault();
  const name = $('#routerName').value.trim();
  if (!/^[\w .-]{1,32}$/.test(name)) return toast('Letters, digits, spaces, dots, dashes and underscores only', 'err');
  withBusy($('button', e.target), async () => {
    await stock('name_set', { name, locale: Sources.rname.data?.locale || 'Home' }, ['rname']);
    toast('Router renamed');
  }).catch(() => {});
});
Views.console = { sources: [], show() { $('#cmd').focus(); } };

on('health', h => {
  if (!h) return;
  $('#sysGauges').innerHTML = healthGauges(h);
  setText('sysUptime', h.uptime_sec != null ? 'up ' + fmtUptime(h.uptime_sec) : '');
  $('#sysKv').innerHTML = kvRows([
    ['Load (1 / 5 / 15 min)', h.load1 != null ? `${h.load1} / ${h.load5} / ${h.load15}` : ''],
    ['Memory available', h.mem_free_real_kb != null ? Math.round(h.mem_free_real_kb / 1024) + ' MB' : ''],
    ['Uptime', h.uptime_sec != null ? fmtUptime(h.uptime_sec) : ''],
    ['Model', [Sources.info.data?.model, Sources.info.data?.firmware && 'firmware ' + Sources.info.data.firmware].filter(Boolean).join(' · ')],
    ['CPE Box', Sources.info.data ? 'v' + Sources.info.data.version : ''],
  ]);
});

bindLedSwitch($('#sysLeds'));
on('info', i => { if (i) renderAccess($('#sysAccess'), i, 'sysAcc'); });
on('ssh', s => {
  if (!s) return;
  setText('sshKey', s.cmd_key);
  setText('sshPw', s.cmd_pw);
  setText('sshHost', s.host);
});

$('#rebootBtn').addEventListener('click', e => {
  if (!confirm('Reboot the router?\n\nInternet and Wi-Fi drop for everyone for 1–2 minutes.')) return;
  withBusy(e.currentTarget, async () => {
    await api('/api/reboot', {});
    setMsg('rebootMsg', 'Rebooting. This page reconnects by itself when the router is back.', 'ok');
    waitForRouter();
  }).catch(() => {});
});

// Poll until the router answers again, then reload everything.
function waitForRouter() {
  const started = Date.now();
  const tick = async () => {
    if (Date.now() - started < 40000) return setTimeout(tick, 5000);
    try {
      await api('/api/system-health');
      setMsg('rebootMsg', 'The router is back.', 'ok');
      toast('Router is back online');
      Object.keys(Sources).forEach(k => { Sources[k].at = 0; });
      Hooks.poll && Hooks.poll();
    } catch (e) {
      if (Date.now() - started > 300000) return setMsg('rebootMsg', "The router hasn't come back after 5 minutes - check its lights and cables.", 'err');
      setTimeout(tick, 5000);
    }
  };
  setTimeout(tick, 5000);
}

$('#pwBtn').addEventListener('click', e => {
  const password = $('#newPw').value;
  if (password.length < 4) return setMsg('pwMsg', 'At least 4 characters', 'err');
  if (!confirm("Change the router's root password?\n\nEvery phone or laptop signed in to this panel will have to sign in again with the new one. Keep it somewhere safe - without it the only way back is a factory reset.")) return;
  withBusy(e.currentTarget, async () => {
    await api('/api/root-password', { password });
    $('#newPw').value = '';
    setMsg('pwMsg', 'Changed', 'ok');
    toast('Root password changed');
  }).catch(err => setMsg('pwMsg', err.message, 'err'));
});

$('#spoofBtn').addEventListener('click', e => {
  if (!confirm('Make the router report firmware version 0.0.1 until the next reboot?\n\nThis only lifts the stock updater\'s downgrade block. Nothing is written to flash.')) return;
  withBusy(e.currentTarget, async () => {
    const d = await api('/api/spoof-version', {});
    setMsg('spoofMsg', 'Router now reports: ' + (d.reported || '0.0.1'), 'ok');
  }).catch(err => setMsg('spoofMsg', err.message, 'err'));
});

// ----------------------------------------------------------- speed test ---
// A speedtest.net-style gauge: the arc spans 240°, with the same piecewise
// log scale as Ookla's (0 5 10 50 100 250 500 750 1000), so slow links don't
// huddle at the left. The backend runs the test in the background; the page
// polls its state 4x a second and animates the needle towards the live rate.

const ST_STOPS = [0, 5, 10, 50, 100, 250, 500, 750, 1000];
const ST_SWEEP = 240; // degrees, from -120 to +120

// position on the scale, 0..1
function stPos(mbps) {
  const v = Math.max(0, Math.min(ST_STOPS[ST_STOPS.length - 1], mbps || 0));
  for (let i = 1; i < ST_STOPS.length; i++) {
    if (v <= ST_STOPS[i]) return (i - 1 + (v - ST_STOPS[i - 1]) / (ST_STOPS[i] - ST_STOPS[i - 1])) / (ST_STOPS.length - 1);
  }
  return 1;
}

(function stBuildTicks() {
  const g = $('#stTicks');
  if (!g) return;
  g.innerHTML = ST_STOPS.map((v, i) => {
    const a = (-120 + i / (ST_STOPS.length - 1) * ST_SWEEP) * Math.PI / 180;
    const sx = 150 + Math.sin(a) * 104, sy = 160 - Math.cos(a) * 104;
    const ex = 150 + Math.sin(a) * 110, ey = 160 - Math.cos(a) * 110;
    const tx = 150 + Math.sin(a) * 90, ty = 160 - Math.cos(a) * 90 + 4;
    return `<line x1="${sx.toFixed(1)}" y1="${sy.toFixed(1)}" x2="${ex.toFixed(1)}" y2="${ey.toFixed(1)}"/><text x="${tx.toFixed(1)}" y="${ty.toFixed(1)}" data-v="${v}">${v}</text>`;
  }).join('');
})();

const ST = { shown: 0, target: 0, raf: 0 };

function stDraw() {
  const p = stPos(ST.shown);
  $('#stNeedle').style.transform = `rotate(${-120 + p * ST_SWEEP}deg)`;
  $('#stFill').style.strokeDasharray = `${(p * 1000).toFixed(1)} 1000`;
  $$('#stTicks text').forEach(t => t.classList.toggle('lit', +t.dataset.v <= ST.shown && ST.shown > 0));
  setText('stLive', ST.shown > 0 ? ST.shown.toFixed(ST.shown < 100 ? 1 : 0) : '—');
}
// ease the needle towards the target between polls, so it moves like a dial
function stAnimate() {
  cancelAnimationFrame(ST.raf);
  const step = () => {
    const d = ST.target - ST.shown;
    if (Math.abs(d) < 0.3) { ST.shown = ST.target; stDraw(); return; }
    ST.shown += d * 0.08;
    stDraw();
    ST.raf = requestAnimationFrame(step);
  };
  ST.raf = requestAnimationFrame(step);
}
function stSetTarget(v) { ST.target = v || 0; stAnimate(); }

const stFmt = v => v == null ? '—' : (+v).toFixed(v < 100 ? 1 : 0);

function stRender(st) {
  const g = $('#stGauge');
  g.dataset.phase = st.phase || '';
  $$('.st-res').forEach(r => r.classList.remove('active'));
  if (st.server) { $('#stServer').hidden = false; setText('stServer', `${st.server.sponsor} · ${st.server.name}`); }
  if (st.ping_ms) setText('stPing', stFmt(st.ping_ms));
  // finished phases keep their number at the top, like speedtest.net
  if (st.down_mbps) setText('stDown', stFmt(st.down_mbps));
  if (st.up_mbps) setText('stUp', stFmt(st.up_mbps));
  switch (st.phase) {
    case 'server': setMsg('stMsg', 'Finding the nearest server…'); setText('stLiveLabel', ''); stSetTarget(0); break;
    case 'ping': setMsg('stMsg', 'Measuring latency…'); setText('stLiveLabel', 'ping'); stSetTarget(0); break;
    case 'download':
      $('#stResDown').classList.add('active');
      setMsg('stMsg', 'Testing download…'); setText('stLiveLabel', '↓ Mbps'); stSetTarget(st.mbps);
      setText('stDown', st.mbps ? stFmt(st.mbps) : '…');
      break;
    case 'upload':
      $('#stResUp').classList.add('active');
      setMsg('stMsg', 'Testing upload…'); setText('stLiveLabel', '↑ Mbps'); stSetTarget(st.mbps);
      setText('stUp', st.mbps ? stFmt(st.mbps) : '…');
      break;
    case 'done': {
      const r = st.result || {};
      setText('stDown', stFmt(r.down_mbps)); setText('stUp', stFmt(r.up_mbps)); setText('stPing', stFmt(r.ping_ms));
      setText('stJitter', r.jitter_ms != null ? `jitter ${r.jitter_ms} ms` : '');
      setText('stLiveLabel', 'Mbps'); stSetTarget(0);
      setMsg('stMsg', 'Done', 'ok');
      setText('stMeta', `${r.server || ''} · ${fmtBytes(r.bytes || 0)} of data used${st.client ? ` · from ${st.client}` : ''}`);
      break;
    }
    case 'error':
      ['stDown', 'stUp'].forEach(id => { if ($('#' + id).textContent === '…') setText(id, '—'); });
      setText('stLiveLabel', 'Mbps'); stSetTarget(0);
      setMsg('stMsg', st.error || 'Speed test failed', 'err');
      break;
  }
}

let stTimer = 0;
async function stPoll() {
  clearTimeout(stTimer);
  try {
    const st = await api('/api/speedtest');
    stRender(st);
    if (st.running) stTimer = setTimeout(stPoll, 250);
    else { $('#stBtn').disabled = false; $('#stBtn').classList.remove('busy'); }
  } catch (err) {
    setMsg('stMsg', err.message, 'err');
    $('#stBtn').disabled = false; $('#stBtn').classList.remove('busy');
  }
}

$('#stBtn').addEventListener('click', async e => {
  const btn = e.currentTarget;
  btn.disabled = true; btn.classList.add('busy');
  ['stDown', 'stUp', 'stPing'].forEach(id => setText(id, '—'));
  setText('stJitter', ''); setText('stMeta', ''); $('#stServer').hidden = true;
  try {
    stRender(await api('/api/speedtest', {}));
    stTimer = setTimeout(stPoll, 250);
  } catch (err) {
    setMsg('stMsg', err.message, 'err');
    btn.disabled = false; btn.classList.remove('busy');
  }
});
// a test started from another tab/device keeps showing here too
Views.system.show = () => { stPoll(); };

// -------------------------------------------------- ping & traceroute ---

$('#diagForm').addEventListener('submit', e => {
  e.preventDefault();
  const host = $('#diagHost').value.trim(), tool = $('#diagTool').value;
  const out = $('#diagOut');
  out.hidden = false;
  out.textContent = tool === 'ping' ? `Pinging ${host}…` : `Tracing the route to ${host} - can take up to a minute…`;
  withBusy($('#diagBtn'), async () => {
    const d = await api('/api/diag', { tool, host });
    out.textContent = d.output || '(no output)';
    if (tool === 'traceroute' && !/^\s*\d+\s+[\d.:a-f]+\s/m.test(d.output || '')) {
      out.textContent += "\n\nNo hop answered. Common on IPv6-only mobile networks (464XLAT, the router's WAN shows 192.0.0.x): the hop replies don't make it back through the translation. Ping still shows whether the host is reachable.";
    }
  }).catch(err => { out.textContent = err.message; });
});
$$('[data-diag-host]').forEach(b => b.addEventListener('click', () => {
  $('#diagHost').value = b.dataset.diagHost;
  $('#diagForm').requestSubmit();
}));

// --------------------------------------------------------------- console ---

async function runConsole(btn, fn, label) {
  const out = $('#out');
  setMsg('runMsg', 'Running…');
  const t0 = performance.now();
  try {
    await withBusy(btn, async () => {
      const d = await fn();
      out.textContent = d.output || '(no output)';
      setMsg('runMsg', `${label} · ${((performance.now() - t0) / 1000).toFixed(1)} s`, 'ok');
    });
  } catch (err) {
    out.textContent = err.message;
    setMsg('runMsg', 'Failed', 'err');
  }
}
$('#runBtn').addEventListener('click', e => {
  const cmd = $('#cmd').value.trim();
  if (!cmd) return $('#cmd').focus();
  runConsole(e.currentTarget, () => api('/api/raw', { cmd }), 'Done');
});
$('#uciBtn').addEventListener('click', e => runConsole(e.currentTarget, () => api('/api/uci-dump'), 'UCI dump'));
$('#clearBtn').addEventListener('click', () => { $('#out').textContent = ''; setMsg('runMsg', ''); });
$('#cmd').addEventListener('keydown', e => {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); $('#runBtn').click(); }
});

// -------------------------------------------------------------- AT modem ---

// Clean up microcom output for the terminal panel: strip the modem's own
// echo of the command back at us and the CR characters that make everything
// look double-spaced in the panel's pre.
function cleanAT(text, sent) {
  if (!text) return '';
  let s = text.replace(/\r\n/g, '\n').replace(/\r/g, '');
  if (sent) {
    const echoed = s.indexOf(sent);
    if (echoed >= 0 && echoed < 3) s = s.slice(echoed + sent.length);
  }
  return s.replace(/^\n+/, '').replace(/\n{3,}/g, '\n\n').trimEnd();
}

async function runAT(btn, cmd) {
  const out = $('#atOut');
  if (!cmd) return $('#atCmd').focus();
  setMsg('atMsg', 'Sending…');
  const t0 = performance.now();
  try {
    await withBusy(btn, async () => {
      const d = await api('/api/at', { cmd });
      out.textContent = cleanAT(d.output, cmd) || '(no reply)';
      setMsg('atMsg', `Done · ${((performance.now() - t0) / 1000).toFixed(1)} s`, 'ok');
    });
  } catch (err) {
    out.textContent = err.message;
    setMsg('atMsg', 'Failed', 'err');
  }
}

$('#atRunBtn').addEventListener('click', e => runAT(e.currentTarget, $('#atCmd').value.trim()));
$('#atClearBtn').addEventListener('click', () => { $('#atOut').textContent = ''; setMsg('atMsg', ''); });
$('#atCmd').addEventListener('keydown', e => {
  if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) { e.preventDefault(); $('#atRunBtn').click(); }
});
// Preset buttons: fill the input and fire.
$$('#v-console [data-at]').forEach(b => b.addEventListener('click', e => {
  const cmd = e.currentTarget.dataset.at;
  $('#atCmd').value = cmd;
  runAT($('#atRunBtn'), cmd);
}));

// ----------------------------------------------------------------- IMEI ---

async function readIMEI(btn) {
  try {
    await withBusy(btn, async () => {
      const d = await api('/api/at', { cmd: 'AT+CGSN' });
      const m = cleanAT(d.output, 'AT+CGSN').match(/\b(\d{15})\b/);
      setText('imeiCurrent', m ? m[1] : '—');
    });
  } catch (err) {
    setText('imeiCurrent', '—');
    setMsg('imeiMsg', err.message, 'err');
  }
}
$('#imeiCheckBtn').addEventListener('click', e => readIMEI(e.currentTarget));
$('#imeiApplyBtn').addEventListener('click', async e => {
  const imei = ($('#imeiNew').value || '').trim();
  if (!/^\d{15}$/.test(imei)) return setMsg('imeiMsg', 'IMEI is 15 digits', 'err');
  if (!confirm(`Write ${imei} as the modem's IMEI?\n\nThis is permanent from the modem's side - it does not revert on a reboot or reset, only by writing another IMEI back in.\n\nThe cellular connection will drop for a few seconds while the modem re-registers.`)) return;
  try {
    await withBusy(e.currentTarget, async () => {
      const d = await api('/api/at', { cmd: `AT+EGMR=1,7,"${imei}"` });
      const reply = cleanAT(d.output, `AT+EGMR=1,7,"${imei}"`);
      if (!/\bOK\b/.test(reply)) throw new Error(reply.trim() || 'Modem rejected the command');
      // Reset the modem's radio so it re-registers with the new identity.
      await api('/api/at', { cmd: 'AT+CFUN=1,1' });
      setMsg('imeiMsg', 'Written · modem is re-registering', 'ok');
      $('#imeiNew').value = '';
      setTimeout(() => readIMEI($('#imeiCheckBtn')), 8000);
    });
  } catch (err) {
    setMsg('imeiMsg', err.message, 'err');
  }
});
// Read current IMEI when the Cellular tab first opens.
window.addEventListener('hashchange', () => {
  if (location.hash === '#cellular' && $('#imeiCurrent').textContent === '—') readIMEI($('#imeiCheckBtn'));
});
if (location.hash === '#cellular') setTimeout(() => readIMEI($('#imeiCheckBtn')), 500);
