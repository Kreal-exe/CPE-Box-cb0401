'use strict';
// Overview: the at-a-glance page. Also holds pieces shared with other
// views: health gauges, the front-lights switch and the "open on your
// phone" block.

Views.overview = { sources: ['cellular', 'connectivity', 'status', 'usage', 'devices', 'health', 'leds', 'info'] };

// Real upstream reachability (ping/DNS from the router) - shown on the overview
// Signal card and the Cellular Connection card, since "Registered" with an IP
// can still mean no working internet.
function renderConnectivity(c) {
  const els = [document.getElementById('ovNet'), document.getElementById('celNetPill')].filter(Boolean);
  if (!els.length) return;
  let cls = 'pill', txt = '—', title = 'Real internet reachability, checked from the router (ping 8.8.8.8)';
  if (c) {
    if (c.online && c.dns) {
      cls += ' good';
      txt = c.latency_ms != null ? `Online · ${Math.round(c.latency_ms)} ms` : 'Online';
    } else if (c.online) {
      cls += ' warn';
      txt = 'DNS issue';
      title = 'Ping to 8.8.8.8 works but DNS resolution fails';
    } else {
      cls += ' bad';
      txt = 'No internet';
      title = 'No reply from 8.8.8.8 - registered but no working data path';
    }
  }
  for (const el of els) { el.className = cls; el.textContent = txt; el.title = title; }
}
on('connectivity', renderConnectivity);

// Which carrier to headline: the 5G one when there is one, else LTE.
function headlineSignal(c) {
  const nr = num(c.rsrp_5g), lte = num(c.rsrp);
  if (nr && /5G/.test(c.network_type || '')) return { v: nr, rat: '5G' };
  if (lte) return { v: lte, rat: 'LTE' };
  return null;
}

function bandTags(band) {
  return (band || '').split('+').filter(Boolean).map(b =>
    `<span class="${/^n/.test(b) ? 'nr' : 'lte'}">${esc(b)}</span>`).join('');
}

const dbm = v => num(v) ? `${num(v)}<small>dBm</small>` : '—';

on('cellular', c => {
  if (!c) return;
  const h = headlineSignal(c);
  const q = h && quality('rsrp', h.v);
  setBars($('#ovBars'), q ? q.level : 0, q && q.level <= 2 ? (q.level === 1 ? 'q-poor' : 'q-fair') : '');
  $('#ovLabel').textContent = q ? q.label : c.registered ? 'No reading' : 'No service';
  $('#ovMeta').textContent = [c.operator, c.network_type, c.roaming ? 'roaming' : ''].filter(Boolean).join(' · ') || '—';

  $('#ovLteRsrp').innerHTML = dbm(c.rsrp);
  $('#ovNrRsrp').innerHTML = dbm(c.rsrp_5g);
  $('#ovLteMeters').innerHTML = meterRow('RSRP', c.rsrp, 'dBm', 'rsrp') + meterRow('RSRQ', c.rsrq, 'dB', 'rsrq') + meterRow('SINR', c.snr, 'dB', 'snr') + meterRow('RSSI', c.rssi, 'dBm', 'rssi');
  setText('ovLteCell', [c.band_primary, c.pci ? 'PCI ' + c.pci : ''].filter(Boolean).join(' · '));
  const has5g = !!num(c.rsrp_5g);
  $('#ovNrBlock').hidden = !has5g;
  if (has5g) {
    $('#ovNrMeters').innerHTML = meterRow('RSRP', c.rsrp_5g, 'dBm', 'rsrp') + meterRow('RSRQ', c.rsrq_5g, 'dB', 'rsrq') + meterRow('SINR', c.snr_5g, 'dB', 'snr');
    setText('ovNrCell', [c.band_5g, c.pci_5g ? 'PCI ' + c.pci_5g : ''].filter(Boolean).join(' · '));
  }
  $('#ovBands').innerHTML = bandTags(c.band);

  renderSim();
});
on('status', () => renderSim());

function renderSim() {
  const c = Sources.cellular.data;
  if (!c) return;
  const sim = $('#simPill');
  sim.className = 'pill ' + (c.sim_status === 'Ready' ? 'good' : 'bad');
  sim.innerHTML = `<span class="dot"></span>${esc(c.sim_status || 'No SIM')}`;
  let pin = c.sim_locked ? 'On' : c.sim_status === 'Ready' ? 'Off' : '—';
  if (c.sim_locked && c.sim_pin_left != null && c.sim_pin_left !== 3) pin += ` (${c.sim_pin_left} tries left)`;
  const rows = [
    ['Phone number', c.sim_number], ['Operator', c.operator + (c.sim_country ? ' · ' + c.sim_country : '')],
    ['APN', c.apn], ['PIN', pin], ['Modem', Sources.status.data?.modem?.modem_model],
  ];
  $('#ovSim').innerHTML = kvRows(rows);
  // Some SIMs carry no number at all - point to where it can be written.
  if (!c.sim_number && c.sim_status === 'Ready') $('#ovSim .kv .v').innerHTML = '<a href="#cellular">Add →</a>';
}

function kvRows(rows) {
  return rows.map(([k, v]) => `<div class="kv"><span class="k">${k}</span><span class="v">${esc(v || '—')}</span></div>`).join('');
}

on('usage', u => {
  if (!u) return;
  // Totals come from the modem itself (mobile.flowstat), which is the
  // only counter on this SoC that catches HW-offloaded traffic. The
  // rx/tx split is that total scaled by trafficd's own rx/tx ratio -
  // trafficd is HW-offload-blind so its absolute bytes are useless, but
  // the ratio between them is a fair estimate of how this line splits
  // download vs upload. Live rate is trafficd's rx_rate/tx_rate directly.
  const big = b => { const [n, unit] = fmtBytesParts(b); return `${esc(n)}<small>${esc(unit)}</small>`; };
  $('#duToday').innerHTML = u.today != null ? big(u.today) : '—';
  setText('duTodayRx', u.today_rx != null ? fmtBytes(u.today_rx) : '—');
  setText('duTodayTx', u.today_tx != null ? fmtBytes(u.today_tx) : '—');
  $('#duMonth').innerHTML = u.month != null ? big(u.month) : '—';
  setText('duMonthRx', u.month_rx != null ? fmtBytes(u.month_rx) : '—');
  setText('duMonthTx', u.month_tx != null ? fmtBytes(u.month_tx) : '—');
  setText('duSince', u.month_start_day ? `since day ${u.month_start_day}` : '');
  const rate = ((u.rx_rate || 0) + (u.tx_rate || 0));
  setText('duRate', rate > 0 ? '↓ ' + fmtBytes(u.rx_rate || 0) + '/s · ↑ ' + fmtBytes(u.tx_rate || 0) + '/s' : 'idle');
});

on('devices', d => {
  if (!d) return;
  const trusted = new Set(d.whitelist || []);
  // A DHCP lease outlives an association by hours - only devices trafficd or
  // the ARP table say are on the network right now count as "online".
  const online = (d.devices || []).filter(x => x.online);
  const unknown = online.filter(x => !trusted.has(x.mac));
  setText('ovOnline', online.length);
  $('#ovUntrusted').innerHTML = unknown.length ? `<span class="q-fair">${unknown.length}</span>` : '0';
  const pill = $('#ovDevPill');
  pill.className = 'pill ' + (unknown.length ? 'warn' : 'good');
  pill.textContent = unknown.length ? 'Review' : 'All trusted';
  const known = online.filter(x => trusted.has(x.mac));
  $('#ovDevRecent').innerHTML = unknown.concat(known).slice(0, 3).map(x => `<div class="dev">
      <div class="avatar ${trusted.has(x.mac) ? 'online' : 'alert'}">${icon(trusted.has(x.mac) ? 'check' : 'alert')}</div>
      <div class="main"><div class="name">${esc(x.hostname || x.mdns_name || x.vendor || 'Unknown device')}</div>
      <div class="meta">${esc([x.ip, x.mac].filter(Boolean).join(' · '))}</div></div></div>`).join('');
  const badge = $('#untrustedCount');
  badge.hidden = !unknown.length;
  badge.textContent = unknown.length;
});

on('health', h => {
  if (!h) return;
  $('#ovGauges').innerHTML = healthGauges(h);
  setText('ovUptime', h.uptime_sec != null ? 'up ' + fmtUptime(h.uptime_sec) : '');
});

function healthGauges(h) {
  const used = h.mem_total_kb ? h.mem_total_kb - h.mem_free_real_kb : null;
  const memPct = used != null ? 100 * used / h.mem_total_kb : null;
  return gauge('CPU', h.cpu_pct, h.cpu_pct != null ? `${Math.round(h.cpu_pct)}<small>%</small>` : null,
      h.load1 != null ? `load ${h.load1}` : '', 60, 85) +
    gauge('Memory', memPct, memPct != null ? `${Math.round(memPct)}<small>%</small>` : null,
      used != null ? `${Math.round(used / 1024)} / ${Math.round(h.mem_total_kb / 1024)} MB` : '', 80, 92) +
    gauge('SoC temp', h.temp_c_max != null ? (h.temp_c_max - 20) / 70 * 100 : null,
      h.temp_c_max != null ? `${Math.round(h.temp_c_max)}<small>°</small>` : null,
      h.temp_c_avg != null ? `avg ${h.temp_c_avg} °C` : '', 71, 86);
}

on('status', s => {
  if (!s || !s.wifi) return;
  $('#ovWifi').innerHTML = [['5', '5 GHz'], ['2.4', '2.4 GHz']].map(([k, label]) => {
    const w = s.wifi[k] || {};
    const up = !!w.channel;
    return `<div class="dev">
      <div class="avatar ${up ? 'online' : ''}"><b>${k}</b></div>
      <div class="main"><div class="name">${esc(w.ssid || label)}</div>
        <div class="meta">${up ? `${label} · ch ${w.channel} · ${w.width_mhz} MHz` : label + ' · off'}</div></div>
      <div class="end">${up ? `${esc(w.clients || 0)} ${icon('devices')}` : ''}</div></div>`;
  }).join('');
});

// ------------------------------------------------ shared: front lights ---

function bindLedSwitch(input) {
  on('leds', (d, err) => {
    if (d) { input.checked = !!d.on; input.disabled = false; }
    else if (err) input.disabled = true;
  });
  input.addEventListener('change', async () => {
    const want = input.checked;
    $$('#ovLeds, #sysLeds').forEach(i => { i.disabled = true; });
    try {
      publish('leds', await api('/api/leds', { on: want }));
      toast(want ? 'Front lights on' : 'Front lights off - they stay off after reboots');
    } catch (e) {
      toast(e.message, 'err');
      input.checked = !want;
    } finally {
      $$('#ovLeds, #sysLeds').forEach(i => { i.disabled = false; });
    }
  });
}
bindLedSwitch($('#ovLeds'));

// ------------------------------------------- shared: open on your phone ---

function renderAccess(el, info, prefix) {
  if (!info.name_url && !info.lan_url) {
    el.innerHTML = `<p class="lead" style="margin:0">Only this computer can open the panel right now: <span class="mono">GUI_BIND</span> in <span class="mono">panel/.env</span> is a local address. Remove that line and restart CPE Box to use it from your phone.</p>`;
    return;
  }
  const alt = info.name_url ? info.name_url.replace('cpe.box', 'cpe.lan') : '';
  const row = (id, url) => `<div class="copy-row"><code id="${prefix}${id}">${esc(url)}</code>
    <button class="icon" data-copy="${prefix}${id}" title="Copy" aria-label="Copy">${icon('copy')}</button></div>`;
  el.innerHTML = `<p class="lead" style="margin:-4px 0 12px">Any phone or laptop on your Wi-Fi can open the panel. It asks for the router's root password.</p>
    ${info.name_url ? row('Name', info.name_url) : ''}
    ${alt && alt !== info.name_url ? row('Alt', alt) : ''}
    ${info.lan_url ? row('Ip', info.lan_url) : ''}
    ${alt && alt !== info.name_url ? `<p class="hint">If <span class="mono">cpe.box</span> doesn't open (browsers with secure DNS skip the router), use <span class="mono">cpe.lan</span> or the address.</p>` : ''}`;
}

on('info', i => { if (i) renderAccess($('#ovAccess'), i, 'ovAcc'); });
