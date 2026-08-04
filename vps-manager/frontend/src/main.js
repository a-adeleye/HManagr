// Frontend entrypoint. Wails generates bindings into ../wailsjs/* on `wails dev`
// / `wails build`, so if you see "Cannot find module" errors, run `wails dev` once
// (or `wails generate module`) to create them.
import './style.css'
import '@xterm/xterm/css/xterm.css'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'

import {
  ListVPS, AddVPS, UpdateVPS, DeleteVPS,
  Connect, TrustHostKey, ForgetHostKey, Disconnect, IsConnected,
  ListFiles, DownloadFile, UploadFile, DeleteRemoteFile, MakeDir, DefaultDownloadDir,
  StatRemoteFile, ChmodRemoteFile, ChownRemoteFile,
  SetSudoPassword, HasSudoPassword,
  FindComposeFile, InspectMigration, RunMigration, DiscoverStacks, RunMultiMigration, DiscoverComposeContext,
  ReadRemoteFile, WriteRemoteFile,
  ListContainers, RestartContainer, StopContainer, StartContainer, ContainerLogs,
  StartShell, WriteShell, ResizeShell, CloseShell,
  ClipboardText, SetClipboardText, ChooseSavePath, ChooseOpenPath,
  ListDBContainers, DBListDatabases, DBListTables, DBTableColumns, DBTableRows,
  DBQuery, DBInsertRow, DBUpdateRow, DBDeleteRow,
  ListDeployments, SaveDeployment, DeleteDeployment, RunDeploy,
  LocalAvailable, LocalUnavailableReason, LocalStartDir,
  ListProjects, SaveProject, DeleteProject,
  InspectTeardown, TeardownStack,
  ListBackupJobs, SaveBackupJob, DeleteBackupJob, RunBackupNow,
  ListBackupSnapshots, ForgetBackupSnapshot, TestBackupTarget, RestoreBackup,
  SystemUsage, PruneDocker, SystemLargestImages,
  HostStats, ContainerStats,
  ProvisionDatabaseEngines, ProvisionDatabase,
  DetectCaddyProxy, ExposeServiceDomain, CloudflareUpsert,
  AskAIAvailable, StartAIQuery, StopAIQuery,
} from '../wailsjs/go/main/App'
import { EventsOn, BrowserOpenURL } from '../wailsjs/runtime/runtime'

// ─────────── State ───────────
const state = {
  vpses: [],
  selectedId: null,
  currentDir: '/',
  connected: false,
  tab: 'files',
  editorPath: null,
}

// Whether the currently-selected environment is the virtual local machine.
const isLocalSel = () => state.selectedId === 'local'

// Local-mode availability (a host POSIX shell exists), resolved once at startup.
state.localAvailable = true
state.localReason = ''

// Which top-level view is showing: the fleet canvas (home) or a server detail.
state.view = 'fleet'           // 'fleet' | 'detail'

// Projects: named server+path bookmarks, listed in the fleet topbar dropdown.
state.projects = []
state.activeProjectId = null   // project currently open (for highlight)
state.projectPath = null       // deploy path the current session is rooted at
state.pendingProjectPath = null // set by openProject, consumed by selectVPS
// When in a project, Docker/DB lists scope to that project's compose stack
// unless the user opts to see everything on the server.
state.dockerShowAll = false
state.dbShowAll = false
// Docker tab: last fetched containers, filtered client-side by a name substring.
state.dockerContainers = []
state.dockerFilter = ''

// Interactive terminal (xterm.js) bound to a single PTY shell at a time.
const term = {
  inst: null,        // Terminal instance (created lazily)
  fit: null,         // FitAddon
  vpsId: null,       // VPS whose shell is currently attached
  rootedPath: null,  // deploy path the attached shell was cd'd into (null = home)
  offOutput: null,   // EventsOn cancel fn for shell:output
  offExit: null,     // EventsOn cancel fn for shell:exit
}

// shQuote single-quotes a POSIX path for safe injection into a shell command.
const shQuote = (p) => "'" + String(p).replace(/'/g, `'\\''`) + "'"

// ─────────── Helpers ───────────
const $ = (id) => document.getElementById(id)

// esc HTML-escapes a value before it goes into innerHTML (container names,
// images, domains, etc. can carry characters that would break markup).
const esc = (s) =>
  String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
const fmtSize = (n) => {
  if (n < 1024) return n + ' B'
  if (n < 1024 ** 2) return (n / 1024).toFixed(1) + ' KB'
  if (n < 1024 ** 3) return (n / 1024 / 1024).toFixed(1) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}
const fmtTime = (unix) => {
  if (!unix) return ''
  const d = new Date(unix * 1000)
  return d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}
const errMsg = (e) => (e && e.message) ? e.message : String(e)

// ─────────── VPS list ───────────
async function refreshVPSList() {
  state.vpses = await ListVPS() || []
  renderFleet()
}

// Resolve whether local mode is usable (a host POSIX shell was found). When not,
// the sidebar entry is dimmed and shows the reason instead of relying solely on
// a Connect-time failure.
async function refreshLocalAvailability() {
  try {
    state.localAvailable = await LocalAvailable()
    state.localReason = state.localAvailable ? '' : (await LocalUnavailableReason())
  } catch {
    state.localAvailable = true
    state.localReason = ''
  }
  renderFleet()
}

// ─────────── Fleet (home view: live server cards) ───────────
//
// The fleet canvas shows one card per server with live CPU / RAM / DISK
// meters, refreshed on a poll loop. Clicking a card expands it to show the
// containers running inside, grouped by compose stack, each with its own live
// usage. "Open" enters the classic detail view (tabs).
const fleet = {
  conn: {},          // vpsId -> live connection state
  stats: {},         // vpsId -> last Host snapshot
  containers: {},    // vpsId -> last Container[] (expanded card only)
  stale: {},         // vpsId -> true when the last stats poll failed
  expandedId: null,  // card currently expanded to show its containers
  timer: null,
  inflight: new Set(),   // vpsIds with a poll in progress (re-entrancy guard)
  connecting: new Set(), // vpsIds with a connect in progress
}
const FLEET_POLL_MS = 6000

// ── view switching ──
function showFleetView() {
  state.view = 'fleet'
  $('main-view').classList.add('hidden')
  $('fleet-view').classList.remove('hidden')
  renderFleet()
  fleetPoll()
}

function showDetailView() {
  state.view = 'detail'
  $('fleet-view').classList.add('hidden')
  $('main-view').classList.remove('hidden')
}

// ── formatting ──
// fmtShortBytes renders a byte count the way the cards' chips want it:
// compact, no space, one decimal only when it matters ("7.8G", "512M").
function fmtShortBytes(n) {
  if (!n || n <= 0) return '0'
  const units = ['B', 'K', 'M', 'G', 'T', 'P']
  let v = n, u = 0
  while (v >= 1024 && u < units.length - 1) { v /= 1024; u++ }
  const s = v >= 10 || u === 0 ? Math.round(v).toString() : v.toFixed(1)
  return s + units[u]
}

const fmtPct = (v) => (v == null || v < 0) ? '—' : Math.round(v) + '%'

function fmtUptime(secs) {
  if (!secs || secs <= 0) return ''
  const d = Math.floor(secs / 86400)
  const h = Math.floor((secs % 86400) / 3600)
  const m = Math.floor((secs % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

// barClass colors a meter by pressure: calm → warm (>70%) → hot (>85%).
function barClass(pct) {
  if (pct == null || pct < 0) return ''
  if (pct > 85) return ' hot'
  if (pct > 70) return ' warm'
  return ''
}

// Each server gets a stable accent color so cards are tellable at a glance.
// The hue is picked by hashing the VPS id, so it never changes across
// sessions or reorderings; Local always gets the neutral slate.
const CARD_HUES = [
  [76, 141, 255],   // blue
  [61, 220, 151],   // green
  [178, 132, 255],  // purple
  [244, 185, 66],   // amber
  [86, 204, 242],   // cyan
  [255, 138, 101],  // coral
  [240, 98, 146],   // pink
  [156, 204, 101],  // lime
]
const LOCAL_HUE = [138, 146, 166] // slate — the odd one out on purpose

function cardAccent(v) {
  if (v.isLocal) return LOCAL_HUE
  let h = 0
  for (let i = 0; i < v.id.length; i++) h = (h * 31 + v.id.charCodeAt(i)) >>> 0
  return CARD_HUES[h % CARD_HUES.length]
}

// svcIcon picks a glyph for a container the way the canvas mock does: globe
// for web-facing things, cylinder for datastores, gear for background work.
function svcIcon(image, name) {
  const s = (String(image) + ' ' + String(name)).toLowerCase()
  if (/postgres|mysql|maria|mongo|redis|valkey|clickhouse|elastic|minio|rabbitmq|kafka|memcache/.test(s)) return '🗃'
  if (/nginx|caddy|traefik|haproxy|storefront|frontend|web|admin|ui/.test(s)) return '🌐'
  if (/worker|queue|cron|job|beat|scheduler/.test(s)) return '⚙'
  return '📦'
}

// ── rendering ──
function renderFleet() {
  const grid = $('fleet-grid')
  grid.innerHTML = ''
  for (const v of state.vpses) grid.appendChild(buildFleetCard(v))
  if (!state.vpses.some((x) => !x.isLocal)) {
    const hint = document.createElement('div')
    hint.className = 'fleet-empty'
    hint.innerHTML = '<h2>No servers yet</h2><p class="muted">Add your first VPS with <b>+ Server</b> — it appears here as a live card.</p>'
    grid.appendChild(hint)
  }
}

// metricChip is one CPU/RAM/DISK cell of a card. Values are patched in place
// by updateFleetCard so the bar animates instead of re-rendering.
function metricChip(kind, label) {
  return `
    <div class="fc-metric" data-metric="${kind}">
      <div class="fc-m-top"><span class="fc-m-label">${label}</span><span class="fc-m-val mono">—</span></div>
      <div class="fc-m-sub mono">&nbsp;</div>
      <div class="fc-m-bar"><i style="width:0%"></i></div>
    </div>`
}

function buildFleetCard(v) {
  const card = document.createElement('div')
  const connected = !!fleet.conn[v.id]
  const localUnavail = v.isLocal && !state.localAvailable
  card.className = 'fleet-card' +
    (fleet.expandedId === v.id ? ' expanded' : '') +
    (connected ? '' : ' offline') +
    (localUnavail ? ' unavailable' : '')
  card.dataset.id = v.id
  // The accent rides a CSS variable as a raw "r, g, b" triplet so the styles
  // can derive washes at any alpha via rgba(var(--ca), …).
  card.style.setProperty('--ca', cardAccent(v).join(', '))
  card.innerHTML = `
    <div class="fc-head">
      <span class="fc-icon">${v.isLocal ? '🖥' : '🌐'}</span>
      <div class="fc-title">
        <span class="fc-name"></span>
        <span class="fc-host mono"></span>
      </div>
      <span class="fc-status"></span>
    </div>
    <div class="fc-meta mono"></div>
    <div class="fc-metrics">
      ${metricChip('cpu', '⛭ CPU')}
      ${metricChip('mem', '▤ RAM')}
      ${metricChip('disk', '⛁ DISK')}
    </div>
    <div class="fc-foot">
      <button class="btn btn-secondary fc-conn"></button>
      <span class="fc-foot-spacer"></span>
      <button class="btn fc-open">Open →</button>
    </div>
    <div class="fc-stacks hidden"></div>
  `
  card.querySelector('.fc-name').textContent = v.name
  card.querySelector('.fc-host').textContent = v.isLocal
    ? (localUnavail ? (state.localReason || 'unavailable on this machine') : 'this machine — no SSH')
    : `${v.user}@${v.host}:${v.port || 22}`
  if (localUnavail) card.title = state.localReason || ''

  const connBtn = card.querySelector('.fc-conn')
  if (fleet.connecting.has(v.id)) {
    connBtn.textContent = 'Connecting…'
    connBtn.disabled = true
  } else {
    connBtn.textContent = connected ? 'Disconnect' : 'Connect'
  }
  connBtn.addEventListener('click', (e) => {
    e.stopPropagation()
    connected ? fleetDisconnect(v.id) : fleetConnect(v.id)
  })
  card.querySelector('.fc-open').addEventListener('click', (e) => {
    e.stopPropagation()
    openDetail(v.id)
  })
  card.addEventListener('click', () => toggleExpand(v.id))
  card.addEventListener('dblclick', () => openDetail(v.id))
  // Clicks inside the stacks area (tiles, empty space between them) shouldn't
  // collapse the card.
  card.querySelector('.fc-stacks').addEventListener('click', (e) => e.stopPropagation())

  updateFleetCard(v.id, card)
  if (fleet.expandedId === v.id) renderStacks(v.id, card)
  return card
}

// updateFleetCard patches a card's live values (status chip, meta line,
// metric chips) without rebuilding its DOM, so bars animate smoothly.
function updateFleetCard(id, card = null) {
  card = card || $('fleet-grid').querySelector(`.fleet-card[data-id="${CSS.escape(id)}"]`)
  if (!card) return
  const connected = !!fleet.conn[id]
  const st = fleet.stats[id]

  const status = card.querySelector('.fc-status')
  if (fleet.connecting.has(id)) {
    status.textContent = '● connecting'
    status.className = 'fc-status connecting'
  } else if (connected) {
    status.textContent = fleet.stale[id] ? '● stale' : '● online'
    status.className = 'fc-status online' + (fleet.stale[id] ? ' stale' : '')
  } else {
    status.textContent = '○ offline'
    status.className = 'fc-status offline'
  }

  const meta = card.querySelector('.fc-meta')
  if (!connected) {
    meta.textContent = 'not connected'
  } else if (!st) {
    meta.textContent = 'reading metrics…'
  } else if (!st.available) {
    meta.textContent = 'metrics unavailable on this host'
  } else {
    const bits = []
    if (st.uptimeSecs > 0) bits.push('up ' + fmtUptime(st.uptimeSecs))
    if (st.load1 >= 0) bits.push('load ' + st.load1.toFixed(2))
    const n = (fleet.containers[id] || []).filter((c) => (c.state || '').toLowerCase() === 'running').length
    if (fleet.expandedId === id && n > 0) bits.push(n + ' running')
    meta.textContent = bits.join(' · ') || ' '
  }

  const setChip = (kind, pct, val, sub) => {
    const chip = card.querySelector(`.fc-metric[data-metric="${kind}"]`)
    if (!chip) return
    chip.querySelector('.fc-m-val').textContent = val
    chip.querySelector('.fc-m-sub').innerHTML = sub || '&nbsp;'
    const bar = chip.querySelector('.fc-m-bar i')
    bar.style.width = (pct != null && pct >= 0 ? Math.min(100, pct) : 0) + '%'
    bar.className = barClass(pct).trim()
  }
  if (connected && st) {
    setChip('cpu', st.cpuPercent, fmtPct(st.cpuPercent),
      st.cores > 0 ? st.cores + (st.cores === 1 ? ' core' : ' cores') : '')
    setChip('mem', st.memPercent, fmtPct(st.memPercent),
      st.memTotal > 0 ? `${fmtShortBytes(st.memUsed)} / ${fmtShortBytes(st.memTotal)}` : '')
    setChip('disk', st.diskPercent, fmtPct(st.diskPercent),
      st.diskTotal > 0 ? `${fmtShortBytes(st.diskUsed)} / ${fmtShortBytes(st.diskTotal)}` : '')
  } else {
    setChip('cpu', -1, '—', ''); setChip('mem', -1, '—', ''); setChip('disk', -1, '—', '')
  }
}

// ── expanded card: containers grouped by compose stack ──
async function toggleExpand(id) {
  if (fleet.expandedId === id) {
    fleet.expandedId = null
    renderFleet()
    return
  }
  fleet.expandedId = id
  renderFleet()
  if (!fleet.conn[id]) return
  const card = $('fleet-grid').querySelector(`.fleet-card[data-id="${CSS.escape(id)}"]`)
  if (card && !fleet.containers[id]) {
    card.querySelector('.fc-stacks').innerHTML = '<p class="muted fc-stacks-msg">Reading containers…</p>'
    card.querySelector('.fc-stacks').classList.remove('hidden')
  }
  try {
    fleet.containers[id] = await ContainerStats(id) || []
    renderStacks(id)
  } catch (e) {
    const c = $('fleet-grid').querySelector(`.fleet-card[data-id="${CSS.escape(id)}"]`)
    if (c && fleet.expandedId === id) {
      const el = c.querySelector('.fc-stacks')
      el.classList.remove('hidden')
      el.innerHTML = ''
      const p = document.createElement('p')
      p.className = 'muted fc-stacks-msg'
      p.textContent = 'Couldn’t read containers: ' + errMsg(e)
      el.appendChild(p)
    }
  }
}

// stackTints rotates through subtle hue washes for the group boxes, echoing
// the canvas mock's blue/green/neutral zones.
const stackTints = [
  ['rgba(76, 141, 255, 0.07)', 'rgba(76, 141, 255, 0.28)'],   // blue
  ['rgba(61, 220, 151, 0.06)', 'rgba(61, 220, 151, 0.26)'],   // green
  ['rgba(178, 132, 255, 0.07)', 'rgba(178, 132, 255, 0.28)'], // purple
  ['rgba(244, 185, 66, 0.06)', 'rgba(244, 185, 66, 0.26)'],   // amber
]

function renderStacks(id, card = null) {
  card = card || $('fleet-grid').querySelector(`.fleet-card[data-id="${CSS.escape(id)}"]`)
  if (!card || fleet.expandedId !== id) return
  const wrap = card.querySelector('.fc-stacks')
  wrap.classList.remove('hidden')
  if (!fleet.conn[id]) {
    delete wrap.dataset.cids
    wrap.innerHTML = '<p class="muted fc-stacks-msg">Connect to see what’s running inside.</p>'
    return
  }
  const containers = fleet.containers[id]
  if (!containers) return
  if (!containers.length) {
    wrap.innerHTML = '<p class="muted fc-stacks-msg">No containers on this server.</p>'
    return
  }

  // If the container set is unchanged, patch tiles in place (smooth bars).
  const ids = containers.map((c) => c.id).sort().join(',')
  if (wrap.dataset.cids === ids) {
    for (const c of containers) patchSvcTile(wrap, c)
    return
  }
  wrap.dataset.cids = ids

  // Group by compose project; standalone containers get their own group.
  const groups = new Map()
  for (const c of containers) {
    const key = c.project || ''
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key).push(c)
  }

  wrap.innerHTML = ''
  let gi = 0
  for (const [project, list] of groups) {
    const [bg, border] = stackTints[gi++ % stackTints.length]
    const group = document.createElement('div')
    group.className = 'stack-group'
    group.style.background = bg
    group.style.borderColor = border
    const running = list.filter((c) => (c.state || '').toLowerCase() === 'running').length
    group.innerHTML = `
      <div class="sg-head">
        <span class="sg-icon">⌗</span>
        <span class="sg-name"></span>
        <span class="sg-count mono">${running}/${list.length} running</span>
      </div>
      <div class="sg-grid"></div>
    `
    group.querySelector('.sg-name').textContent = project || 'containers'
    const sgGrid = group.querySelector('.sg-grid')
    for (const c of list) sgGrid.appendChild(buildSvcTile(id, c))
    wrap.appendChild(group)
  }
}

function buildSvcTile(vpsId, c) {
  const tile = document.createElement('div')
  const running = (c.state || '').toLowerCase() === 'running'
  tile.className = 'svc-tile' + (running ? '' : ' stopped')
  tile.dataset.cid = c.id
  // image:tag without the registry path, like the mock's "storefront:2.4.0"
  const shortImage = String(c.image || '').split('/').pop()
  tile.innerHTML = `
    <div class="svc-head">
      <span class="svc-icon">${svcIcon(c.image, c.name)}</span>
      <span class="svc-name"></span>
      <span class="svc-badge"></span>
    </div>
    <div class="svc-image mono"></div>
    <div class="svc-metrics">
      <div class="svc-m" data-m="cpu">
        <span class="svc-m-label">CPU</span><span class="svc-m-val mono">—</span>
        <div class="fc-m-bar"><i style="width:0%"></i></div>
      </div>
      <div class="svc-m" data-m="mem">
        <span class="svc-m-label">RAM</span><span class="svc-m-val mono">—</span>
        <div class="fc-m-bar"><i style="width:0%"></i></div>
      </div>
    </div>
    <div class="svc-actions"></div>
  `
  tile.querySelector('.svc-name').textContent = c.service || c.name
  tile.querySelector('.svc-image').textContent = shortImage
  tile.querySelector('.svc-image').title = `${c.name} · ${c.image}`

  const actions = tile.querySelector('.svc-actions')
  const act = (label, fn) => {
    const b = document.createElement('button')
    b.textContent = label
    b.addEventListener('click', async (e) => {
      e.stopPropagation()
      b.disabled = true
      try { await fn() } catch (err) { alert('Action failed: ' + errMsg(err)) }
      b.disabled = false
    })
    actions.appendChild(b)
  }
  const refresh = async () => {
    fleet.containers[vpsId] = await ContainerStats(vpsId) || []
    renderStacks(vpsId)
  }
  if (running) {
    act('restart', async () => { await RestartContainer(vpsId, c.id); await refresh() })
    act('stop', async () => { await StopContainer(vpsId, c.id); await refresh() })
  } else {
    act('start', async () => { await StartContainer(vpsId, c.id); await refresh() })
  }
  act('logs', async () => {
    await openDetail(vpsId)
    await showLogs(c.id, c.name)
  })

  patchSvcTile(tile.parentElement || tile, c, tile)
  return tile
}

// patchSvcTile updates one tile's badge + metric values in place.
function patchSvcTile(scope, c, tile = null) {
  tile = tile || scope.querySelector(`.svc-tile[data-cid="${CSS.escape(c.id)}"]`)
  if (!tile) return
  const stateName = (c.state || '').toLowerCase()
  const running = stateName === 'running'
  tile.classList.toggle('stopped', !running)
  const badge = tile.querySelector('.svc-badge')
  badge.textContent = running ? 'RUN' : (stateName || 'stopped').toUpperCase()
  badge.className = 'svc-badge ' + (running ? 'run' : (stateName === 'restarting' ? 'restarting' : 'stopped'))
  badge.title = c.status || ''

  const set = (kind, pct, text) => {
    const m = tile.querySelector(`.svc-m[data-m="${kind}"]`)
    if (!m) return
    m.querySelector('.svc-m-val').textContent = text
    const bar = m.querySelector('.fc-m-bar i')
    bar.style.width = (pct >= 0 ? Math.min(100, pct) : 0) + '%'
    bar.className = barClass(pct).trim()
  }
  set('cpu', running ? c.cpuPercent : -1, running ? fmtPct(c.cpuPercent) : '—')
  const memText = running && c.memUsed > 0
    ? `${fmtShortBytes(c.memUsed)}${c.memLimit > 0 ? ' / ' + fmtShortBytes(c.memLimit) : ''}`
    : '—'
  set('mem', running ? c.memPercent : -1, memText)
}

// ── polling ──
function startFleetPolling() {
  if (fleet.timer) return
  fleet.timer = setInterval(fleetPoll, FLEET_POLL_MS)
  fleetPoll()
}

async function fleetPoll() {
  await Promise.all(state.vpses.map((v) => fleetPollOne(v)))
}

async function fleetPollOne(v) {
  if (fleet.inflight.has(v.id) || fleet.connecting.has(v.id)) return
  fleet.inflight.add(v.id)
  try {
    let conn = false
    try { conn = await IsConnected(v.id) } catch {}
    const changed = !!fleet.conn[v.id] !== conn
    fleet.conn[v.id] = conn
    if (changed && state.view === 'fleet') {
      // Structural change (Connect ↔ Disconnect button) → rebuild the grid.
      renderFleet()
    }
    if (!conn) {
      delete fleet.stats[v.id]
      delete fleet.containers[v.id]
      return
    }
    try {
      fleet.stats[v.id] = await HostStats(v.id)
      fleet.stale[v.id] = false
    } catch {
      fleet.stale[v.id] = true
    }
    if (state.view === 'fleet') updateFleetCard(v.id)
    if (fleet.expandedId === v.id) {
      try {
        fleet.containers[v.id] = await ContainerStats(v.id) || []
        if (state.view === 'fleet') renderStacks(v.id)
      } catch {}
    }
  } finally {
    fleet.inflight.delete(v.id)
  }
}

// ── card actions ──
async function fleetConnect(id) {
  if (fleet.connecting.has(id)) return
  fleet.connecting.add(id)
  renderFleet()
  try {
    await connectFlow(id)
    fleet.conn[id] = true
    if (state.selectedId === id) {
      state.connected = true
      updateStatusUI()
    }
  } catch (e) {
    alert('Connection failed: ' + errMsg(e))
  } finally {
    fleet.connecting.delete(id)
    renderFleet()
    fleetPoll()
  }
}

async function fleetDisconnect(id) {
  if (term.vpsId === id) {
    await detachShell()
    resetTerminalPanel()
  }
  try { await Disconnect(id) } catch {}
  fleet.conn[id] = false
  delete fleet.stats[id]
  delete fleet.containers[id]
  if (state.selectedId === id) {
    state.connected = false
    updateStatusUI()
  }
  renderFleet()
}

// openDetail enters the classic tabbed view for a server.
async function openDetail(id) {
  await selectVPS(id)
}

async function selectVPS(id) {
  // Switching environments: tear down any interactive shell attached to the
  // previous one so its PTY stream doesn't keep writing into a hidden terminal
  // (the local env's terminal branch never calls openShell, which is what would
  // otherwise have detached it).
  if (state.selectedId && state.selectedId !== id) await detachShell()
  state.selectedId = id
  const v = state.vpses.find((x) => x.id === id)
  if (!v) return
  try {
    state.connected = await IsConnected(id)
  } catch {
    state.connected = false
  }
  fleet.conn[id] = state.connected
  showDetailView()
  $('vps-name').textContent = v.name
  $('vps-host').textContent = v.isLocal ? 'this machine — no SSH' : `${v.user}@${v.host}:${v.port || 22}`
  // Edit/Delete are meaningless for the virtual local entry.
  $('edit-vps-btn').classList.toggle('hidden', !!v.isLocal)
  $('delete-vps-btn').classList.toggle('hidden', !!v.isLocal)
  // Opening a project roots the session at its deploy path; a plain
  // server/local selection clears any project context.
  if (state.pendingProjectPath) {
    state.currentDir = state.pendingProjectPath
    state.projectPath = state.pendingProjectPath
    state.pendingProjectPath = null
  } else {
    state.projectPath = null
    state.activeProjectId = null
    if (v.isLocal) {
      try { state.currentDir = await LocalStartDir() } catch { state.currentDir = '/' }
    } else {
      // Stacks live under /opt by convention, so the browser opens there;
      // loadFiles falls back to / on hosts without it.
      state.currentDir = '/opt'
    }
  }
  // Each (re)selection starts scoped: a project shows its own stack, a plain
  // server shows everything (projectPath null makes the scope a no-op).
  state.dockerShowAll = false
  state.dbShowAll = false
  state.dockerContainers = []
  state.dockerFilter = ''
  $('docker-search').value = ''
  updateStatusUI()
  dbResetState()
  if (state.connected) {
    loadCurrentTab()
  } else {
    $('files-list').innerHTML = '<p class="muted center-msg">Connect to browse files.</p>'
    $('docker-list').innerHTML = '<p class="muted center-msg">Connect to view containers.</p>'
    resetTerminalPanel()
    dbShowEmpty('Connect to browse databases.')
  }
}

function updateStatusUI() {
  const pill = $('status-pill')
  if (state.connected) {
    pill.textContent = 'Connected'
    pill.className = 'status-pill connected'
    $('connect-btn').classList.add('hidden')
    $('disconnect-btn').classList.remove('hidden')
  } else {
    pill.textContent = 'Disconnected'
    pill.className = 'status-pill disconnected'
    $('connect-btn').classList.remove('hidden')
    $('disconnect-btn').classList.add('hidden')
  }
}

// ─────────── Projects ───────────
// A project is a named server+path bookmark. Opening one selects its server
// (reusing a live connection) and roots the session at the deploy path.
async function refreshProjects() {
  try { state.projects = await ListProjects() || [] } catch { state.projects = [] }
  if (!$('fleet-proj-menu').classList.contains('hidden')) renderProjectMenu()
}

// serverLabel describes the server a project points at, for the project row.
function serverLabel(vpsId) {
  const v = state.vpses.find((x) => x.id === vpsId)
  if (!v) return '⚠ missing server'
  return v.isLocal ? 'Local' : `${v.user}@${v.host}`
}

// activeProject returns the project currently open, or null.
function activeProject() {
  return state.projects.find((p) => p.id === state.activeProjectId) || null
}

// The fleet topbar's Projects dropdown. Rows open the project (connects and
// roots the session at its deploy path); hover reveals edit/delete.
function renderProjectMenu() {
  const menu = $('fleet-proj-menu')
  menu.innerHTML = ''
  if (!state.projects.length) {
    menu.innerHTML = '<div class="fleet-proj-empty muted">No projects yet — use + Project.</div>'
    return
  }
  for (const p of state.projects) {
    const row = document.createElement('div')
    row.className = 'fleet-proj-row' + (state.activeProjectId === p.id ? ' active' : '')
    row.innerHTML = `
      <div class="fp-main">
        <span class="fp-name"></span>
        <span class="fp-server muted"></span>
      </div>
      <div class="fp-path mono"></div>
      <div class="proj-actions"></div>
    `
    row.querySelector('.fp-name').textContent = p.name
    row.querySelector('.fp-server').textContent = serverLabel(p.vpsId)
    row.querySelector('.fp-path').textContent = p.path
    const actions = row.querySelector('.proj-actions')
    const ed = document.createElement('button')
    ed.textContent = 'Edit'
    ed.onclick = (e) => { e.stopPropagation(); toggleProjectMenu(false); openProjectModal(p) }
    const del = document.createElement('button')
    del.textContent = 'Del'
    del.onclick = async (e) => {
      e.stopPropagation()
      if (!confirm(`Delete project "${p.name}"? (The server itself is kept.)`)) return
      await DeleteProject(p.id)
      if (state.activeProjectId === p.id) state.activeProjectId = null
      refreshProjects()
    }
    actions.append(ed, del)
    row.addEventListener('click', () => { toggleProjectMenu(false); openProject(p.id) })
    menu.appendChild(row)
  }
}

function toggleProjectMenu(show) {
  const menu = $('fleet-proj-menu')
  const want = show ?? menu.classList.contains('hidden')
  if (want) renderProjectMenu()
  menu.classList.toggle('hidden', !want)
}

async function openProject(projectId) {
  const p = state.projects.find((x) => x.id === projectId)
  if (!p) return
  const v = state.vpses.find((x) => x.id === p.vpsId)
  if (!v) { alert('This project’s server no longer exists. Edit the project to point it at a server.'); return }
  state.activeProjectId = projectId
  // selectVPS consumes this to root the session at the deploy path.
  state.pendingProjectPath = p.path
  await selectVPS(p.vpsId)
  // Connect like a server: reuse a live connection, otherwise connect now.
  if (!state.connected) await doConnect()
}

// ── Project modal ──
function projServerOptions() {
  return state.vpses.map((v) =>
    `<option value="${v.id}">${v.isLocal ? v.name : `${v.name} (${v.user}@${v.host})`}</option>`).join('')
}

function toggleProjectServerMode(mode) {
  $('project-existing-field').classList.toggle('hidden', mode !== 'existing')
  $('project-new-fields').classList.toggle('hidden', mode !== 'new')
}

function toggleNpAuth(type) {
  $('np-key-field').classList.toggle('hidden', type !== 'key')
  $('np-password-field').classList.toggle('hidden', type !== 'password')
}

function openProjectModal(project = null) {
  const form = $('project-form')
  form.reset()
  $('project-vps').innerHTML = projServerOptions()
  $('np-port').value = '22'
  $('np-authType').value = 'key'
  toggleNpAuth('key')
  if (project) {
    $('project-modal-title').textContent = 'Edit Project'
    form.elements.namedItem('id').value = project.id
    form.elements.namedItem('name').value = project.name
    form.elements.namedItem('path').value = project.path
    form.elements.namedItem('database').value = project.database || ''
    form.elements.namedItem('vpsId').value = project.vpsId
    $('project-server-mode').value = 'existing' // server already exists when editing
    $('project-vps').value = project.vpsId
  } else {
    $('project-modal-title').textContent = 'New Project'
    form.elements.namedItem('id').value = ''
    form.elements.namedItem('vpsId').value = ''
    $('project-server-mode').value = 'existing'
    if (state.selectedId) $('project-vps').value = state.selectedId
  }
  toggleProjectServerMode($('project-server-mode').value)
  $('project-modal').classList.remove('hidden')
}

async function submitProjectForm(e) {
  e.preventDefault()
  const form = $('project-form')
  const mode = $('project-server-mode').value
  const p = {
    id: form.elements.namedItem('id').value || '',
    name: form.elements.namedItem('name').value.trim(),
    path: form.elements.namedItem('path').value.trim(),
    database: form.elements.namedItem('database').value.trim(),
    vpsId: '',
  }
  // newServer is only consulted by the backend when vpsId is empty.
  let newServer = { host: '' }
  if (mode === 'existing') {
    p.vpsId = $('project-vps').value
  } else {
    newServer = {
      name: $('np-name').value.trim() || $('np-host').value.trim(),
      host: $('np-host').value.trim(),
      port: parseInt($('np-port').value || '22', 10),
      user: $('np-user').value.trim(),
      authType: $('np-authType').value,
      keyPath: $('np-keyPath').value.trim(),
      password: $('np-password').value,
    }
  }
  try {
    await SaveProject(p, newServer)
    $('project-modal').classList.add('hidden')
    await refreshVPSList()   // an inline server may have just been created
    await refreshProjects()
  } catch (err) {
    alert('Save failed: ' + errMsg(err))
  }
}

// ─────────── Connection ───────────

// connectFlow dials a VPS, walking the user through the host-key trust
// prompts (trust-on-first-use, changed-key warning). Resolves when connected,
// throws when the user declines or the dial fails. UI-agnostic: both the
// fleet cards and the detail topbar use it and update their own chrome.
async function connectFlow(id, trust = false) {
  const res = await (trust ? TrustHostKey : Connect)(id)
  // Unknown host key (first connection): show the fingerprint and let the user
  // decide whether to trust it (trust-on-first-use).
  if (res && res.needsTrust) {
    const ok = confirm(
      `Unknown SSH host key for ${res.host}\n\n` +
      `${res.keyType} fingerprint:\n${res.fingerprint}\n\n` +
      `This is the first time connecting to this server. ` +
      `Verify the fingerprint matches the server, then trust it and continue?`)
    if (ok) return connectFlow(id, true)
    throw new Error('Host key not trusted — connection cancelled.')
  }
  // Changed host key: a possible man-in-the-middle. Make the user opt in loudly.
  if (res && res.keyChanged) {
    const ok = confirm(
      `⚠ WARNING: the SSH host key for ${res.host} has CHANGED.\n\n` +
      `New ${res.keyType} fingerprint:\n${res.fingerprint}\n\n` +
      `This happens if the server was rebuilt — but also if the connection is ` +
      `being intercepted (man-in-the-middle). Only continue if you KNOW the ` +
      `server legitimately changed.\n\nForget the old key and trust the new one?`)
    if (ok) {
      await ForgetHostKey(id)
      return connectFlow(id, true)
    }
    throw new Error('Host key changed — connection refused.')
  }
}

async function doConnect() {
  if (!state.selectedId) return
  const btn = $('connect-btn')
  btn.textContent = 'Connecting…'
  btn.disabled = true
  $('status-pill').textContent = 'Connecting'
  $('status-pill').className = 'status-pill connecting'
  try {
    await connectFlow(state.selectedId)
    state.connected = true
    fleet.conn[state.selectedId] = true
    updateStatusUI()
    loadCurrentTab()
  } catch (e) {
    alert('Connection failed: ' + errMsg(e))
    state.connected = false
    updateStatusUI()
  } finally {
    btn.textContent = 'Connect'
    btn.disabled = false
  }
}

async function doDisconnect() {
  if (!state.selectedId) return
  await detachShell()
  resetTerminalPanel()
  try { await Disconnect(state.selectedId) } catch {}
  state.connected = false
  fleet.conn[state.selectedId] = false
  delete fleet.stats[state.selectedId]
  delete fleet.containers[state.selectedId]
  updateStatusUI()
}

// Restore the terminal panel to its disconnected state.
function resetTerminalPanel() {
  if (term.inst) term.inst.reset()
  $('terminal').classList.add('hidden')
  $('terminal-placeholder').classList.remove('hidden')
}

function loadCurrentTab() {
  // The migration and deploy tabs pick their own VPSes, so they stay
  // accessible regardless of the selected VPS's connection state.
  if (state.tab === 'migration') {
    migOpen()
    return
  }
  if (state.tab === 'deploy') {
    deployOpen()
    return
  }
  if (state.tab === 'db') {
    dbOpen()
    return
  }
  if (state.tab === 'backups') {
    backupsOpen()
    return
  }
  if (state.tab === 'cleanup') {
    cleanupOpen()
    return
  }
  if (state.tab === 'domains') {
    domainsOpen()
    return
  }
  if (state.tab === 'provision') {
    provisionOpen()
    return
  }
  if (state.tab === 'maintenance') {
    maintenanceOpen()
    return
  }
  if (state.tab === 'ask') {
    askOpen()
    return
  }
  if (!state.connected) return
  if (state.tab === 'files') {
    if (!state.currentDir) state.currentDir = '/'
    $('files-path').value = state.currentDir
    loadFiles(state.currentDir)
  } else if (state.tab === 'docker') {
    loadContainers()
  } else if (state.tab === 'terminal') {
    if (isLocalSel()) {
      // No in-app PTY for local — point the user at their own terminal.
      $('terminal').classList.add('hidden')
      const ph = $('terminal-placeholder')
      ph.classList.remove('hidden')
      ph.textContent = 'The interactive terminal isn’t available in local mode — use your own terminal on this machine. Docker, Databases, Deploy and Files all work here.'
    } else {
      openShell()
    }
  }
}

// ─────────── Files ───────────
async function loadFiles(dir) {
  if (!state.connected) return
  const list = $('files-list')
  list.innerHTML = '<p class="muted center-msg">Loading…</p>'
  try {
    const files = await ListFiles(state.selectedId, dir) || []
    state.currentDir = dir
    $('files-path').value = dir
    renderFiles(files)
  } catch (e) {
    // The default start dir is /opt; a host without it lands at / instead of
    // an error. Deliberate: also applies when /opt is typed by hand.
    if (dir === '/opt' && !isLocalSel()) {
      loadFiles('/')
      return
    }
    list.innerHTML = `<p class="muted center-msg">Error: ${errMsg(e)}</p>`
  }
}

function renderFiles(files) {
  const list = $('files-list')
  list.innerHTML = ''
  if (!files.length) {
    list.innerHTML = '<p class="muted center-msg">Empty directory.</p>'
    return
  }
  files.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1
    return a.name.localeCompare(b.name)
  })
  for (const f of files) {
    const row = document.createElement('div')
    row.className = 'file-row'
    row.innerHTML = `
      <span class="icon">${f.isDir ? '📁' : '📄'}</span>
      <span class="name"></span>
      <span class="size"></span>
      <span class="modified"></span>
      <span class="actions"></span>
    `
    row.querySelector('.name').textContent = f.name
    row.querySelector('.size').textContent = f.isDir ? '—' : fmtSize(f.size)
    row.querySelector('.modified').textContent = fmtTime(f.modTime)

    const actions = row.querySelector('.actions')
    if (!f.isDir) {
      const ed = document.createElement('button')
      ed.textContent = 'Edit'
      ed.onclick = (e) => { e.stopPropagation(); openEditor(f.path) }
      actions.appendChild(ed)

      const dl = document.createElement('button')
      dl.textContent = 'Download'
      dl.onclick = (e) => { e.stopPropagation(); downloadFile(f.path, f.name) }
      actions.appendChild(dl)
    }
    const perm = document.createElement('button')
    perm.textContent = 'Perms'
    perm.onclick = (e) => { e.stopPropagation(); openPermsModal(f.path) }
    actions.appendChild(perm)

    const del = document.createElement('button')
    del.textContent = 'Delete'
    del.onclick = (e) => { e.stopPropagation(); deleteFile(f.path) }
    actions.appendChild(del)

    if (f.isDir) {
      row.addEventListener('click', () => loadFiles(f.path))
    }
    list.appendChild(row)
  }
}

async function downloadFile(remotePath, name) {
  try {
    const defaultDir = await DefaultDownloadDir()
    const localPath = await ChooseSavePath(defaultDir, name)
    if (!localPath) return
    await DownloadFile(state.selectedId, remotePath, localPath)
  } catch (e) {
    alert('Download failed: ' + errMsg(e))
  }
}

async function uploadFile() {
  try {
    const localPath = await ChooseOpenPath()
    if (!localPath) return
    const filename = localPath.split(/[\\/]/).pop()
    const dir = state.currentDir.endsWith('/') ? state.currentDir : state.currentDir + '/'
    await UploadFile(state.selectedId, localPath, dir + filename)
    loadFiles(state.currentDir)
  } catch (e) {
    alert('Upload failed: ' + errMsg(e))
  }
}

function createDir() {
  if (!state.connected) return
  $('mkdir-parent').textContent = state.currentDir
  $('mkdir-name').value = ''
  $('mkdir-sudo').checked = false
  $('mkdir-modal').classList.remove('hidden')
  setTimeout(() => $('mkdir-name').focus(), 0)
}

async function submitMkdir(e) {
  e.preventDefault()
  const name = $('mkdir-name').value.trim()
  if (!name) return
  // Reject path separators so users don't accidentally create nested paths
  // (or absolute ones) when they just want a folder in the current directory.
  if (/[\\/]/.test(name)) {
    alert('Folder name cannot contain "/" or "\\".')
    return
  }
  const useSudo = $('mkdir-sudo').checked
  if (useSudo && !(await ensureSudoPassword(state.selectedId))) return
  const dir = state.currentDir.endsWith('/') ? state.currentDir : state.currentDir + '/'
  try {
    await MakeDir(state.selectedId, dir + name, useSudo)
    $('mkdir-modal').classList.add('hidden')
    loadFiles(state.currentDir)
  } catch (err) {
    // If sudo couldn't authenticate non-interactively, prompt for a password
    // and retry once so the user doesn't have to know to enable it upfront.
    if (useSudo && /sudo password required/i.test(errMsg(err))) {
      if (await ensureSudoPassword(state.selectedId, true)) {
        try {
          await MakeDir(state.selectedId, dir + name, true)
          $('mkdir-modal').classList.add('hidden')
          loadFiles(state.currentDir)
          return
        } catch (e2) { alert('Create folder failed: ' + errMsg(e2)); return }
      }
    }
    alert('Create folder failed: ' + errMsg(err))
  }
}

// ensureSudoPassword resolves true once a sudo password is cached for the VPS,
// or false if the user cancels. force=true reshows the prompt even if one is
// already cached (used after a "password required" failure when the cached
// value turned out to be wrong).
async function ensureSudoPassword(id, force = false) {
  if (!force && (await HasSudoPassword(id))) return true
  return new Promise((resolve) => {
    const modal = $('sudo-modal')
    const form = $('sudo-form')
    const pw = $('sudo-pw')
    pw.value = ''
    modal.classList.remove('hidden')
    setTimeout(() => pw.focus(), 0)

    const cleanup = () => {
      modal.classList.add('hidden')
      form.removeEventListener('submit', onSubmit)
      $('sudo-cancel').removeEventListener('click', onCancel)
    }
    const onSubmit = async (e) => {
      e.preventDefault()
      const v = pw.value
      if (!v) return
      try { await SetSudoPassword(id, v) } catch {}
      cleanup(); resolve(true)
    }
    const onCancel = () => { cleanup(); resolve(false) }
    form.addEventListener('submit', onSubmit)
    $('sudo-cancel').addEventListener('click', onCancel)
  })
}

// ─────────── Permissions (chmod / chown) ───────────
// permsTarget holds the path currently being edited so the form submit handler
// (wired once at startup) knows what to act on.
let permsTarget = null

async function openPermsModal(path) {
  if (!state.connected) return
  permsTarget = path
  $('perms-path').textContent = path
  // Show defaults while the stat call is in flight.
  $('perms-mode').value = ''
  $('perms-owner').value = ''
  $('perms-group').value = ''
  $('perms-sudo').checked = false
  $('perms-modal').classList.remove('hidden')
  try {
    const info = await StatRemoteFile(state.selectedId, path)
    $('perms-mode').value = info.mode
    $('perms-owner').value = info.owner || String(info.uid)
    $('perms-group').value = info.group || String(info.gid)
  } catch (e) {
    alert('Stat failed: ' + errMsg(e))
    closePermsModal()
  }
}

function closePermsModal() {
  $('perms-modal').classList.add('hidden')
  permsTarget = null
}

async function submitPerms(e) {
  e.preventDefault()
  if (!permsTarget) return
  const path = permsTarget
  const mode = $('perms-mode').value.trim()
  const owner = $('perms-owner').value.trim()
  const group = $('perms-group').value.trim()
  const useSudo = $('perms-sudo').checked
  if (useSudo && !(await ensureSudoPassword(state.selectedId))) return
  const apply = async () => {
    if (mode) await ChmodRemoteFile(state.selectedId, path, mode, useSudo)
    if (owner || group) await ChownRemoteFile(state.selectedId, path, owner, group, useSudo)
  }
  try {
    await apply()
    closePermsModal()
    loadFiles(state.currentDir)
  } catch (err) {
    if (useSudo && /sudo password required/i.test(errMsg(err))) {
      if (await ensureSudoPassword(state.selectedId, true)) {
        try { await apply(); closePermsModal(); loadFiles(state.currentDir); return }
        catch (e2) { alert('Update failed: ' + errMsg(e2)); return }
      }
    }
    alert('Update failed: ' + errMsg(err))
  }
}

async function deleteFile(path) {
  if (!confirm(`Delete ${path}?\nThis cannot be undone.`)) return
  try {
    await DeleteRemoteFile(state.selectedId, path)
    loadFiles(state.currentDir)
  } catch (e) {
    alert('Delete failed: ' + errMsg(e))
  }
}

function goUp() {
  const dir = state.currentDir
  if (dir === '/' || dir === '') return
  // A Windows drive root ("C:" or "C:/") has nothing above it — don't fall
  // through to a bare "/", which on Windows silently jumps to the current
  // drive's root and loses the drive in the breadcrumb.
  if (/^[A-Za-z]:\/?$/.test(dir)) return
  const parts = dir.replace(/\/$/, '').split('/')
  parts.pop()
  let parent = parts.join('/') || '/'
  // "C:" addresses the per-drive working dir, not the root — normalize to "C:/".
  if (/^[A-Za-z]:$/.test(parent)) parent += '/'
  loadFiles(parent)
}

// ─────────── File editor ───────────
async function openEditor(remotePath) {
  const saveBtn = $('editor-save')
  $('editor-filename').textContent = remotePath
  $('editor-textarea').value = 'Loading…'
  $('editor-textarea').disabled = true
  saveBtn.disabled = true
  state.editorPath = remotePath
  $('editor-modal').classList.remove('hidden')
  try {
    const content = await ReadRemoteFile(state.selectedId, remotePath)
    $('editor-textarea').value = content
    $('editor-textarea').disabled = false
    saveBtn.disabled = false
    $('editor-textarea').focus()
  } catch (e) {
    alert('Cannot open file: ' + errMsg(e))
    closeEditor()
  }
}

function closeEditor() {
  $('editor-modal').classList.add('hidden')
  state.editorPath = null
}

async function saveEditor() {
  if (!state.editorPath) return
  const saveBtn = $('editor-save')
  saveBtn.textContent = 'Saving…'
  saveBtn.disabled = true
  try {
    await WriteRemoteFile(state.selectedId, state.editorPath, $('editor-textarea').value)
    closeEditor()
    loadFiles(state.currentDir)
  } catch (e) {
    alert('Save failed: ' + errMsg(e))
  } finally {
    saveBtn.textContent = 'Save'
    saveBtn.disabled = false
  }
}

// ─────────── Docker ───────────
async function loadContainers() {
  const list = $('docker-list')
  // In a project, try to scope to its compose stack unless "All" is on.
  const inProject = !!state.projectPath
  const wantScope = inProject && !state.dockerShowAll
  $('docker-scope-row').classList.toggle('hidden', !inProject)
  $('docker-all').checked = state.dockerShowAll
  const note = $('docker-scope-note')
  note.classList.add('hidden')

  list.innerHTML = '<p class="muted center-msg">Loading…</p>'
  try {
    let containers = await ListContainers(state.selectedId, wantScope ? state.projectPath : '') || []
    if (wantScope && containers.length === 0) {
      // Nothing matched this project's compose stack — its containers may not be
      // compose-managed, or were started from a different directory. Fall back
      // to all rather than leaving the list mysteriously empty.
      containers = await ListContainers(state.selectedId, '') || []
      note.textContent = `couldn’t match a compose stack at ${state.projectPath} — showing all`
      note.classList.remove('hidden')
    } else if (wantScope) {
      note.textContent = `scoped to ${state.projectPath}`
      note.classList.remove('hidden')
    }
    state.dockerContainers = containers
    renderContainers()
  } catch (e) {
    state.dockerContainers = []
    list.innerHTML = `<p class="muted center-msg">Error: ${errMsg(e)}</p>`
  }
}

function renderContainers() {
  const list = $('docker-list')
  list.innerHTML = ''
  const all = state.dockerContainers
  if (!all.length) {
    list.innerHTML = '<p class="muted center-msg">No containers found.</p>'
    return
  }
  // Plain case-insensitive substring match on the name, not an exact match.
  const q = state.dockerFilter.trim().toLowerCase()
  const containers = q
    ? all.filter((c) => (c.names || '').toLowerCase().includes(q))
    : all
  if (!containers.length) {
    list.innerHTML = `<p class="muted center-msg">No containers match “${esc(q)}”.</p>`
    return
  }
  for (const c of containers) {
    const row = document.createElement('div')
    row.className = 'container-row'
    const stateName = (c.state || '').toLowerCase()
    row.innerHTML = `
      <div class="info">
        <div class="name-line">
          <span class="name"></span>
          <span class="state ${stateName}"></span>
        </div>
        <div class="meta"></div>
        <div class="meta ports"></div>
      </div>
      <div class="actions"></div>
    `
    row.querySelector('.name').textContent = c.names
    row.querySelector('.state').textContent = c.state
    row.querySelector('.meta:not(.ports)').textContent = `${c.image} · ${c.id} · ${c.status}`
    const portsEl = row.querySelector('.ports')
    if (c.ports) portsEl.textContent = c.ports
    else portsEl.style.display = 'none'

    const actions = row.querySelector('.actions')
    if (stateName === 'running') {
      addAction(actions, 'Restart', () => containerAction(RestartContainer, c.id))
      addAction(actions, 'Stop', () => containerAction(StopContainer, c.id))
    } else {
      addAction(actions, 'Start', () => containerAction(StartContainer, c.id))
    }
    addAction(actions, 'Logs', () => showLogs(c.id, c.names))
    list.appendChild(row)
  }
}

function addAction(parent, label, fn) {
  const b = document.createElement('button')
  b.className = 'btn btn-secondary'
  b.textContent = label
  b.onclick = fn
  parent.appendChild(b)
}

async function containerAction(fn, id) {
  try {
    await fn(state.selectedId, id)
    loadContainers()
  } catch (e) {
    alert('Action failed: ' + errMsg(e))
  }
}

async function showLogs(id, name) {
  try {
    const logs = await ContainerLogs(state.selectedId, id, 200)
    // Logs are a read-only view: stop any interactive shell so keystrokes
    // don't leak into it, then switch to the terminal panel without
    // auto-starting a new shell.
    await detachShell()
    setActiveTab('terminal')
    termPrint(`\x1b[2m--- logs ${name} (last 200 lines) ---\x1b[0m\r\n`)
    termPrint(logs.endsWith('\n') ? logs : logs + '\n')
    fitTerm()
  } catch (e) {
    alert('Logs failed: ' + errMsg(e))
  }
}

// ─────────── Databases ───────────
// Browses database engines running in docker containers on the connected VPS:
// containers → databases → tables → rows, plus a free-form SQL editor.
// Credentials never reach the frontend — the backend sniffs them from the
// container env and is addressed purely by container ID.
const dbs = {
  containers: [],
  containerId: null,
  database: null,
  tables: [],
  table: null,     // { schema, name }
  columns: [],     // [{ name, type, nullable, isPk }]
  page: 0,
  pageSize: 50,
  total: 0,
  lastResult: null, // QueryResult backing the current grid (for edit/delete)
  mode: 'tables',
}

const dbSudo = () => $('db-sudo').checked

function dbResetState() {
  dbs.containers = []
  dbs.containerId = null
  dbs.database = null
  dbs.tables = []
  dbs.table = null
  dbs.columns = []
  dbs.page = 0
  dbs.lastResult = null
  $('db-container').innerHTML = ''
  $('db-database').innerHTML = ''
  $('db-table-list').innerHTML = ''
  $('db-grid').innerHTML = '<p class="muted center-msg">Pick a table.</p>'
  $('db-sql-result').innerHTML = ''
}

function dbShowEmpty(msg) {
  $('db-empty').textContent = msg
  $('db-empty').classList.remove('hidden')
  $('db-tables-view').classList.add('hidden')
  $('db-sql-view').classList.add('hidden')
}

function dbShowMode() {
  $('db-empty').classList.add('hidden')
  $('db-tables-view').classList.toggle('hidden', dbs.mode !== 'tables')
  $('db-sql-view').classList.toggle('hidden', dbs.mode !== 'sql')
  $('db-mode-tables').classList.toggle('active', dbs.mode === 'tables')
  $('db-mode-sql').classList.toggle('active', dbs.mode === 'sql')
}

async function dbOpen() {
  if (!state.connected) {
    dbShowEmpty('Connect to browse databases.')
    return
  }
  dbShowMode()
  await dbLoadContainers()
}

async function dbLoadContainers() {
  const sel = $('db-container')
  // In a project, try to scope to its compose stack unless "Show all" is on.
  const inProject = !!state.projectPath
  const wantScope = inProject && !state.dbShowAll
  $('db-scope-row').classList.toggle('hidden', !inProject)
  $('db-all').checked = state.dbShowAll
  sel.innerHTML = '<option>Loading…</option>'
  try {
    dbs.containers = await ListDBContainers(state.selectedId, dbSudo(), wantScope ? state.projectPath : '') || []
    if (wantScope && dbs.containers.length === 0) {
      // No DB containers matched the project's stack — fall back to all so the
      // tab isn't empty (e.g. when the DB runs outside the project's compose).
      dbs.containers = await ListDBContainers(state.selectedId, dbSudo(), '') || []
    }
  } catch (e) {
    dbShowEmpty('Could not list containers: ' + errMsg(e))
    return
  }
  sel.innerHTML = ''
  if (!dbs.containers.length) {
    dbShowEmpty('No database containers found (postgres / mysql / mariadb images are detected).')
    return
  }
  dbShowMode()
  for (const c of dbs.containers) {
    const opt = document.createElement('option')
    opt.value = c.id
    opt.textContent = `${c.name} · ${c.engine}${c.state !== 'running' ? ' (' + c.state + ')' : ''}`
    opt.disabled = c.state !== 'running'
    sel.appendChild(opt)
  }
  const prev = dbs.containers.find((c) => c.id === dbs.containerId && c.state === 'running')
  const pick = prev || dbs.containers.find((c) => c.state === 'running')
  if (!pick) {
    dbShowEmpty('Database containers exist but none are running. Start one from the Docker tab.')
    return
  }
  sel.value = pick.id
  dbs.containerId = pick.id
  await dbLoadDatabases()
}

// projectDatabaseFor resolves the database a project is pinned to for container
// c: the project's explicit Database, else the container's sniffed default.
function projectDatabaseFor(c) {
  const p = activeProject()
  return (p && p.database) || (c && c.defaultDb) || ''
}

async function dbLoadDatabases() {
  const c = dbs.containers.find((x) => x.id === dbs.containerId)
  const sel = $('db-database')
  sel.innerHTML = '<option>Loading…</option>'
  try {
    const list = await DBListDatabases(state.selectedId, dbs.containerId, dbSudo()) || []
    if (!list.length) {
      sel.innerHTML = ''
      $('db-table-list').innerHTML = '<p class="muted center-msg">No databases.</p>'
      return
    }
    // In a project, pin the dropdown to the project's database (unless "Show
    // all" is on, or that database isn't present on this container).
    const projectDb = projectDatabaseFor(c)
    const scoped = state.projectPath && !state.dbShowAll && projectDb && list.includes(projectDb)
    const shown = scoped ? [projectDb] : list
    sel.innerHTML = ''
    for (const name of shown) {
      const opt = document.createElement('option')
      opt.value = name
      opt.textContent = name
      sel.appendChild(opt)
    }
    const prefer = scoped ? projectDb
      : (dbs.database && shown.includes(dbs.database)) ? dbs.database
      : (c && shown.includes(c.defaultDb)) ? c.defaultDb : shown[0]
    sel.value = prefer
    dbs.database = prefer
    await dbLoadTables()
  } catch (e) {
    sel.innerHTML = ''
    dbGridMessage($('db-grid'), 'err', 'List databases failed: ' + errMsg(e))
  }
}

async function dbLoadTables() {
  const list = $('db-table-list')
  list.innerHTML = '<p class="muted center-msg" style="padding:20px 8px;">Loading…</p>'
  try {
    dbs.tables = await DBListTables(state.selectedId, dbs.containerId, dbs.database, dbSudo()) || []
  } catch (e) {
    list.innerHTML = ''
    dbGridMessage($('db-grid'), 'err', 'List tables failed: ' + errMsg(e))
    return
  }
  list.innerHTML = ''
  if (!dbs.tables.length) {
    list.innerHTML = '<p class="muted" style="padding:12px;font-size:12px;">No tables.</p>'
    $('db-grid').innerHTML = '<p class="muted center-msg">This database has no tables.</p>'
    return
  }
  const sameTable = dbs.table && dbs.tables.some((t) => t.schema === dbs.table.schema && t.name === dbs.table.name)
  for (const t of dbs.tables) {
    const div = document.createElement('div')
    div.className = 'db-table-item'
    div.textContent = (t.schema && t.schema !== 'public' && t.schema !== dbs.database) ? `${t.schema}.${t.name}` : t.name
    div.title = `${t.schema}.${t.name}`
    div.addEventListener('click', () => dbSelectTable(t))
    list.appendChild(div)
  }
  await dbSelectTable(sameTable ? dbs.table : dbs.tables[0])
}

async function dbSelectTable(t) {
  dbs.table = t
  dbs.page = 0
  const items = Array.from($('db-table-list').children)
  items.forEach((el, i) => el.classList.toggle('active',
    dbs.tables[i] && dbs.tables[i].schema === t.schema && dbs.tables[i].name === t.name))
  $('db-grid-title').textContent = `${t.schema}.${t.name}`
  try {
    dbs.columns = await DBTableColumns(state.selectedId, dbs.containerId, dbs.database, t.schema, t.name, dbSudo()) || []
  } catch (e) {
    dbs.columns = []
    dbGridMessage($('db-grid'), 'err', 'Columns failed: ' + errMsg(e))
    return
  }
  await dbLoadRows()
}

const dbHasPK = () => dbs.columns.some((c) => c.isPk)

async function dbLoadRows() {
  const grid = $('db-grid')
  grid.innerHTML = '<p class="muted center-msg">Loading…</p>'
  const t = dbs.table
  try {
    const res = await DBTableRows(state.selectedId, dbs.containerId, dbs.database,
      t.schema, t.name, dbs.pageSize, dbs.page * dbs.pageSize, dbSudo())
    dbs.total = res.total || 0
    dbs.lastResult = res
    renderDbGrid(grid, res, { editable: dbHasPK() })
    const from = dbs.page * dbs.pageSize + 1
    const to = Math.min(dbs.total, (dbs.page + 1) * dbs.pageSize)
    $('db-grid-count').textContent = dbs.total ? `${from}–${to} of ${dbs.total} rows` : 'empty'
    $('db-page').textContent = `p.${dbs.page + 1}`
    $('db-prev').disabled = dbs.page === 0
    $('db-next').disabled = to >= dbs.total
    $('db-insert').classList.remove('hidden')
    if (!dbHasPK() && dbs.total > 0) {
      $('db-grid-count').textContent += ' · no primary key — rows are read-only here, use SQL'
    }
  } catch (e) {
    dbGridMessage(grid, 'err', 'Query failed: ' + errMsg(e))
  }
}

function dbGridMessage(el, cls, text) {
  el.innerHTML = ''
  const div = document.createElement('div')
  div.className = 'grid-msg ' + cls
  div.textContent = text
  el.appendChild(div)
}

// renderDbGrid renders a QueryResult. With opts.editable, per-row Edit/Delete
// actions are added (requires the grid to be the current table page).
function renderDbGrid(el, res, opts = {}) {
  el.innerHTML = ''
  if (!res.columns || !res.columns.length) {
    dbGridMessage(el, 'ok', res.message || 'OK')
    return
  }
  const table = document.createElement('table')
  const thead = document.createElement('thead')
  const hr = document.createElement('tr')
  const pkNames = new Set(dbs.columns.filter((c) => c.isPk).map((c) => c.name))
  for (const col of res.columns) {
    const th = document.createElement('th')
    th.textContent = col
    if (opts.editable && pkNames.has(col)) {
      const b = document.createElement('span')
      b.className = 'pk-badge'
      b.textContent = '🔑'
      th.appendChild(b)
    }
    hr.appendChild(th)
  }
  if (opts.editable) hr.appendChild(document.createElement('th'))
  thead.appendChild(hr)
  table.appendChild(thead)

  const tbody = document.createElement('tbody')
  for (const row of res.rows || []) {
    const tr = document.createElement('tr')
    row.forEach((v) => {
      const td = document.createElement('td')
      if (v === null || v === undefined) {
        td.className = 'null'
        td.textContent = 'NULL'
      } else {
        td.textContent = v
        td.title = v.length > 60 ? v : ''
      }
      tr.appendChild(td)
    })
    if (opts.editable) {
      const td = document.createElement('td')
      td.className = 'row-actions'
      const ed = document.createElement('button')
      ed.textContent = 'Edit'
      ed.onclick = () => openRowModal('edit', res, row)
      const del = document.createElement('button')
      del.textContent = 'Del'
      del.onclick = () => dbDeleteRow(res, row)
      td.append(ed, del)
      tr.appendChild(td)
    }
    tbody.appendChild(tr)
  }
  table.appendChild(tbody)
  el.appendChild(table)
  if ((res.rows || []).length === 0) {
    const div = document.createElement('div')
    div.className = 'grid-msg'
    div.textContent = 'No rows.'
    el.appendChild(div)
  }
}

// pkMapFor builds { pkCol: originalValue } for the WHERE clause of an
// update/delete, using the row as it was loaded.
function pkMapFor(res, row) {
  const pk = {}
  for (const c of dbs.columns) {
    if (!c.isPk) continue
    const idx = res.columns.indexOf(c.name)
    if (idx === -1) return null
    pk[c.name] = row[idx]
  }
  return Object.keys(pk).length ? pk : null
}

async function dbDeleteRow(res, row) {
  const pk = pkMapFor(res, row)
  if (!pk) return alert('No primary key — delete via the SQL editor instead.')
  const desc = Object.entries(pk).map(([k, v]) => `${k}=${v === null ? 'NULL' : v}`).join(', ')
  if (!confirm(`Delete row where ${desc}?\nThis cannot be undone.`)) return
  try {
    const msg = await DBDeleteRow(state.selectedId, dbs.containerId, dbs.database,
      dbs.table.schema, dbs.table.name, pk, dbSudo())
    await dbLoadRows()
    $('db-grid-count').textContent += ` · ${msg}`
  } catch (e) {
    alert('Delete failed: ' + errMsg(e))
  }
}

// Row modal state: what a save should do.
let rowModalCtx = null // { mode: 'insert'|'edit', res, row, original: {col: value|null} }

function openRowModal(mode, res = null, row = null) {
  if (!dbs.columns.length) return
  rowModalCtx = { mode, res, row, original: {} }
  $('db-row-title').textContent = mode === 'insert' ? 'Insert row' : 'Edit row'
  $('db-row-table').textContent = `${dbs.table.schema}.${dbs.table.name}`
  const wrap = $('db-row-fields')
  wrap.innerHTML = ''
  for (const col of dbs.columns) {
    let value = null
    let present = false
    if (mode === 'edit' && res) {
      const idx = res.columns.indexOf(col.name)
      if (idx !== -1) { value = row[idx]; present = true }
    }
    rowModalCtx.original[col.name] = present ? value : undefined

    const field = document.createElement('div')
    field.className = 'db-row-field'
    const label = document.createElement('label')
    label.textContent = `${col.name} · ${col.type}${col.isPk ? ' · PK' : ''}`
    const input = document.createElement('input')
    input.type = 'text'
    input.dataset.col = col.name
    input.value = value === null || value === undefined ? '' : value
    input.disabled = value === null && mode === 'edit'
    label.appendChild(input)
    const nullToggle = document.createElement('label')
    nullToggle.className = 'null-toggle'
    const cb = document.createElement('input')
    cb.type = 'checkbox'
    cb.dataset.nullFor = col.name
    cb.checked = mode === 'edit' ? value === null : false
    cb.addEventListener('change', () => { input.disabled = cb.checked })
    nullToggle.append(cb, document.createTextNode('NULL'))
    field.append(label, nullToggle)
    wrap.appendChild(field)
  }
  $('db-row-modal').classList.remove('hidden')
}

function closeRowModal() {
  $('db-row-modal').classList.add('hidden')
  rowModalCtx = null
}

async function submitRowModal(e) {
  e.preventDefault()
  if (!rowModalCtx) return
  const { mode, res, row } = rowModalCtx
  const values = {}
  for (const col of dbs.columns) {
    const input = document.querySelector(`#db-row-fields input[data-col="${CSS.escape(col.name)}"]`)
    const nullCb = document.querySelector(`#db-row-fields input[data-null-for="${CSS.escape(col.name)}"]`)
    if (!input) continue
    const newVal = nullCb && nullCb.checked ? null : input.value
    if (mode === 'edit') {
      // Send only changed columns: avoids clobbering values that don't
      // round-trip through text cleanly (binary, high-precision types).
      const orig = rowModalCtx.original[col.name]
      if (orig === undefined) continue
      if (newVal === orig) continue
      values[col.name] = newVal
    } else {
      // Insert: skip blank non-null fields so column defaults apply.
      if (newVal === '' ) continue
      values[col.name] = newVal
    }
  }
  try {
    let msg
    if (mode === 'insert') {
      if (!Object.keys(values).length) return alert('Nothing to insert — fill at least one field.')
      msg = await DBInsertRow(state.selectedId, dbs.containerId, dbs.database,
        dbs.table.schema, dbs.table.name, values, dbSudo())
    } else {
      if (!Object.keys(values).length) { closeRowModal(); return }
      const pk = pkMapFor(res, row)
      if (!pk) return alert('No primary key — edit via the SQL editor instead.')
      msg = await DBUpdateRow(state.selectedId, dbs.containerId, dbs.database,
        dbs.table.schema, dbs.table.name, pk, values, dbSudo())
    }
    closeRowModal()
    await dbLoadRows()
    $('db-grid-count').textContent += ` · ${msg}`
  } catch (err) {
    alert('Save failed: ' + errMsg(err))
  }
}

async function dbRunSQL() {
  const sql = $('db-sql-input').value.trim()
  if (!sql) return
  if (!dbs.containerId) return alert('Pick a database container first.')
  const out = $('db-sql-result')
  out.innerHTML = '<p class="muted center-msg">Running…</p>'
  const btn = $('db-sql-run')
  btn.disabled = true
  try {
    const res = await DBQuery(state.selectedId, dbs.containerId, dbs.database, sql, dbSudo())
    renderDbGrid(out, res, { editable: false })
  } catch (e) {
    dbGridMessage(out, 'err', errMsg(e))
  } finally {
    btn.disabled = false
  }
}

// ─────────── Deploy (GitHub repo → VPS via docker compose) ───────────
const dep = {
  list: [],
  runningId: null,
  logOff: null,
  doneOff: null,
}

function deployOpen() {
  $('deploy-run-view').classList.add('hidden')
  $('deploy-list-view').classList.remove('hidden')
  deployRefresh()
}

async function deployRefresh() {
  try {
    dep.list = await ListDeployments() || []
  } catch (e) {
    dep.list = []
  }
  renderDeployList()
}

// deploySourceLabel describes how a deployment gets its code, since the
// same repoUrl now drives two different behaviors: cloned onto the target
// (classic), or cloned+built only locally with just the image published to
// the target (publish mode) — leaving it ambiguous would misleadingly imply
// the target sees source when it doesn't.
function deploySourceLabel(d) {
  if (!d.repoUrl) return 'existing directory (no git)'
  const repo = `${d.repoUrl} @ ${d.branch || 'main'}`
  return d.publishLocalPath ? `${repo} → built & published locally (image only reaches target)` : repo
}

function renderDeployList() {
  const list = $('deploy-list')
  list.innerHTML = ''
  if (!dep.list.length) {
    list.innerHTML = '<p class="muted center-msg">No deployments yet. Connect a GitHub repo with “+ New deployment”.</p>'
    return
  }
  for (const d of dep.list) {
    const vps = state.vpses.find((v) => v.id === d.vpsId)
    const card = document.createElement('div')
    card.className = 'deploy-card'
    card.innerHTML = `
      <div class="info">
        <div class="title-line">
          <span class="name"></span>
          <span class="deploy-status hidden"></span>
        </div>
        <div class="meta repo"></div>
        <div class="meta target"></div>
        <div class="meta last hidden"></div>
      </div>
      <div class="actions"></div>
    `
    card.querySelector('.name').textContent = d.name
    const st = card.querySelector('.deploy-status')
    const status = dep.runningId === d.id ? 'running' : d.lastStatus
    if (status) {
      st.textContent = status
      st.className = 'deploy-status ' + status
    }
    card.querySelector('.repo').textContent = deploySourceLabel(d)
    card.querySelector('.target').textContent = `${vps ? vps.name : '⚠ missing VPS'} : ${d.path}${d.useSudo ? ' · sudo' : ''}`
    const last = card.querySelector('.last')
    if (d.lastDeploy) {
      last.classList.remove('hidden')
      const when = new Date(d.lastDeploy)
      last.textContent = `last: ${when.toLocaleString()}${d.lastCommit ? ' · ' + d.lastCommit : ''}`
    }
    const actions = card.querySelector('.actions')
    const deployBtn = document.createElement('button')
    deployBtn.className = 'btn'
    deployBtn.textContent = 'Deploy'
    deployBtn.disabled = !!dep.runningId
    deployBtn.onclick = () => startDeploy(d)
    const editBtn = document.createElement('button')
    editBtn.className = 'btn btn-secondary'
    editBtn.textContent = 'Edit'
    editBtn.onclick = () => openDeployModal(d)
    const delBtn = document.createElement('button')
    delBtn.className = 'btn btn-danger'
    delBtn.textContent = 'Delete'
    delBtn.onclick = async () => {
      if (!confirm(`Delete deployment "${d.name}"? (Nothing is removed from the VPS.)`)) return
      await DeleteDeployment(d.id)
      deployRefresh()
    }
    actions.append(deployBtn, editBtn, delBtn)
    list.appendChild(card)
  }
}

function addEnvRow(key = '', value = '', secret = false) {
  const row = document.createElement('div')
  row.className = 'deploy-env-row'
  const k = document.createElement('input')
  k.type = 'text'
  k.placeholder = 'KEY'
  k.value = key
  k.className = 'env-key'
  const v = document.createElement('input')
  v.type = secret ? 'password' : 'text'
  v.placeholder = 'value'
  v.value = value
  v.className = 'env-value'
  const secretToggle = document.createElement('label')
  secretToggle.className = 'secret-toggle'
  const cb = document.createElement('input')
  cb.type = 'checkbox'
  cb.checked = secret
  cb.className = 'env-secret'
  cb.addEventListener('change', () => { v.type = cb.checked ? 'password' : 'text' })
  secretToggle.append(cb, document.createTextNode('secret'))
  const del = document.createElement('button')
  del.type = 'button'
  del.className = 'env-del'
  del.textContent = '✕'
  del.onclick = () => row.remove()
  row.append(k, v, secretToggle, del)
  $('deploy-env-rows').appendChild(row)
}

// parseDotEnv reads .env-format text (KEY=value per line) into {key, value}
// pairs. Supports '#' full-line comments, blank lines, an optional leading
// "export ", and single/double-quoted values (double-quoted values unescape
// \n, \" and \\ same as dotenv/shell); everything else is taken literally so
// values with '$', '=' or URLs survive untouched.
function parseDotEnv(text) {
  const out = []
  for (const rawLine of text.replace(/\r\n?/g, '\n').split('\n')) {
    let line = rawLine.trim()
    if (!line || line.startsWith('#')) continue
    if (line.startsWith('export ')) line = line.slice('export '.length).trim()
    const eq = line.indexOf('=')
    if (eq === -1) continue
    const key = line.slice(0, eq).trim()
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) continue
    let value = line.slice(eq + 1).trim()
    if (value.length >= 2 &&
      ((value[0] === '"' && value.endsWith('"')) || (value[0] === "'" && value.endsWith("'")))) {
      const quote = value[0]
      value = value.slice(1, -1)
      if (quote === '"') value = value.replace(/\\n/g, '\n').replace(/\\"/g, '"').replace(/\\\\/g, '\\')
    }
    out.push({ key, value })
  }
  return out
}

// looksSecret guesses which imported keys are sensitive so they land
// pre-masked; the checkbox is still there for the user to correct either way.
function looksSecret(key) {
  return /SECRET|PASSWORD|PRIVATE|TOKEN|_KEY|APIKEY|CREDENTIAL/i.test(key)
}

// importEnvRows upserts parsed pairs into the existing row list — a key
// already present gets its value updated in place, a new key gets a new row —
// so importing a file composes with vars added by hand instead of wiping them.
// Returns how many pairs were parsed (0 surfaces as "nothing to import").
function importEnvRows(text) {
  const pairs = parseDotEnv(text)
  const rows = $('deploy-env-rows')
  for (const { key, value } of pairs) {
    let matched = null
    for (const row of rows.children) {
      if (row.querySelector('.env-key').value.trim() === key) { matched = row; break }
    }
    if (matched) {
      matched.querySelector('.env-value').value = value
    } else {
      addEnvRow(key, value, looksSecret(key))
    }
  }
  return pairs.length
}

function openDeployModal(d = null) {
  const form = $('deploy-form')
  form.reset()
  $('deploy-env-rows').innerHTML = ''
  $('deploy-env-import-text').value = ''
  $('deploy-env-import').classList.add('hidden')
  // VPS choices come from the saved list (local included — deploying a repo to
  // this machine is valid).
  const sel = $('deploy-vps')
  sel.innerHTML = state.vpses.map((v) =>
    `<option value="${v.id}">${v.isLocal ? v.name : `${v.name} (${v.user}@${v.host})`}</option>`).join('')
  if (d) {
    $('deploy-modal-title').textContent = 'Edit deployment'
    for (const [k, val] of Object.entries(d)) {
      const f = form.elements.namedItem(k)
      if (f && f.type !== 'checkbox') f.value = val ?? ''
    }
    $('deploy-sudo').checked = !!d.useSudo
    for (const ev of d.envVars || []) addEnvRow(ev.key, ev.value, ev.secret)
  } else {
    $('deploy-modal-title').textContent = 'New deployment'
    form.elements.namedItem('id').value = ''
    form.elements.namedItem('branch').value = 'main'
    if (state.selectedId) sel.value = state.selectedId
  }
  $('deploy-modal').classList.remove('hidden')
}

async function submitDeployForm(e) {
  e.preventDefault()
  const form = $('deploy-form')
  const fd = new FormData(form)
  const d = {
    id: fd.get('id') || '',
    name: (fd.get('name') || '').trim(),
    vpsId: fd.get('vpsId'),
    repoUrl: (fd.get('repoUrl') || '').trim(),
    branch: (fd.get('branch') || '').trim() || 'main',
    path: (fd.get('path') || '').trim(),
    composeFile: (fd.get('composeFile') || '').trim(),
    githubToken: fd.get('githubToken') || '',
    registryHost: (fd.get('registryHost') || '').trim(),
    registryUsername: (fd.get('registryUsername') || '').trim(),
    registryToken: fd.get('registryToken') || '',
    publishLocalPath: (fd.get('publishLocalPath') || '').trim(),
    publishBuildContext: (fd.get('publishBuildContext') || '').trim(),
    publishImageRepo: (fd.get('publishImageRepo') || '').trim(),
    publishImageEnvVar: (fd.get('publishImageEnvVar') || '').trim(),
    useSudo: $('deploy-sudo').checked,
    envVars: [],
  }
  for (const row of $('deploy-env-rows').children) {
    const key = row.querySelector('.env-key').value.trim()
    if (!key) continue
    d.envVars.push({
      key,
      value: row.querySelector('.env-value').value,
      secret: row.querySelector('.env-secret').checked,
    })
  }
  // Preserve status fields on edit so the card history survives a re-save.
  const existing = dep.list.find((x) => x.id === d.id)
  if (existing) {
    d.lastDeploy = existing.lastDeploy
    d.lastStatus = existing.lastStatus
    d.lastCommit = existing.lastCommit
  }
  try {
    await SaveDeployment(d)
    $('deploy-modal').classList.add('hidden')
    deployRefresh()
  } catch (err) {
    alert('Save failed: ' + errMsg(err))
  }
}

async function startDeploy(d) {
  if (dep.runningId) return
  dep.runningId = d.id
  $('deploy-list-view').classList.add('hidden')
  $('deploy-run-view').classList.remove('hidden')
  $('deploy-run-title').textContent = `Deploying ${d.name}`
  const log = $('deploy-log')
  const source = deploySourceLabel(d)
  log.textContent = `⚡ ${source} → ${d.path}\n` +
    (d.useSudo ? '⚡ Sudo: ON\n\n' : '⚡ Sudo: off\n\n')
  const outcome = $('deploy-outcome')
  outcome.classList.add('hidden')

  const cleanup = () => {
    if (dep.logOff) { dep.logOff(); dep.logOff = null }
    if (dep.doneOff) { dep.doneOff(); dep.doneOff = null }
    dep.runningId = null
  }
  dep.logOff = EventsOn('deploy:log:' + d.id, (line) => {
    log.textContent += line + '\n'
    log.scrollTop = log.scrollHeight
  })
  dep.doneOff = EventsOn('deploy:done:' + d.id, (errStr) => {
    if (errStr) {
      outcome.textContent = '✗ Deploy failed: ' + errStr
      outcome.className = 'deploy-outcome err'
    } else {
      outcome.textContent = '✓ Deploy complete.'
      outcome.className = 'deploy-outcome ok'
    }
    outcome.classList.remove('hidden')
    cleanup()
    deployRefresh()
  })

  try {
    await RunDeploy(d.id)
  } catch (e) {
    outcome.textContent = '✗ Failed to start: ' + errMsg(e)
    outcome.className = 'deploy-outcome err'
    outcome.classList.remove('hidden')
    cleanup()
  }
}

// ─────────── Terminal (interactive PTY shell over xterm.js) ───────────

// Decode a base64 chunk from the backend back into the raw bytes xterm expects.
// PTY output is base64-encoded on the wire so non-UTF-8 / split sequences
// survive the JSON event transport intact.
function b64ToBytes(b64) {
  const bin = atob(b64)
  const bytes = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i)
  return bytes
}

// Create the xterm instance once and mount it. Keystrokes are forwarded to
// whichever shell is currently attached (term.vpsId).
function ensureTerm() {
  if (term.inst) return
  term.inst = new Terminal({
    fontFamily: 'var(--mono), Menlo, Consolas, monospace',
    fontSize: 13,
    cursorBlink: true,
    scrollback: 5000,
    theme: { background: '#050709', foreground: '#d4d7dd', cursor: '#4f9cf9' },
  })
  term.fit = new FitAddon()
  term.inst.loadAddon(term.fit)
  term.inst.open($('terminal'))
  term.inst.onData((d) => {
    if (term.vpsId) WriteShell(term.vpsId, d).catch(() => {})
  })

  // Clipboard: the WebView doesn't deliver native paste to xterm, so wire it
  // explicitly through Wails' clipboard. Conventions follow Windows Terminal:
  //   Ctrl+V / Ctrl+Shift+V  → paste
  //   Ctrl+Shift+C           → copy selection
  //   Ctrl+C                 → copy if text is selected, else interrupt (^C)
  term.inst.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown') return true
    const ctrl = e.ctrlKey || e.metaKey
    if (!ctrl) return true
    const key = e.key.toLowerCase()
    if (key === 'v') {
      pasteIntoShell()
      return false
    }
    if (key === 'c') {
      const sel = term.inst.getSelection()
      if (sel && sel.length > 0) {
        copySelection(sel)
        return false
      }
      // No selection → fall through so Ctrl+C reaches the shell as interrupt.
    }
    return true
  })

  // Right-click: copy a selection if there is one, otherwise paste.
  $('terminal').addEventListener('contextmenu', (e) => {
    e.preventDefault()
    const sel = term.inst.getSelection()
    if (sel && sel.length > 0) copySelection(sel)
    else pasteIntoShell()
  })
}

async function pasteIntoShell() {
  if (!term.vpsId) return
  try {
    const text = await ClipboardText()
    if (text) WriteShell(term.vpsId, text).catch(() => {})
  } catch {}
}

function copySelection(sel) {
  SetClipboardText(sel).catch(() => {})
  term.inst.clearSelection()
}

// Recompute size to fill the panel, then tell the remote PTY about it.
function fitTerm() {
  if (!term.inst || !term.fit) return
  try {
    term.fit.fit()
  } catch { return }
  if (term.vpsId) {
    const { cols, rows } = term.inst
    ResizeShell(term.vpsId, cols, rows).catch(() => {})
  }
}

// Open (or re-open) an interactive shell for the selected VPS and stream it
// into the terminal. Called when the Terminal tab is shown while connected.
async function openShell() {
  if (!state.connected || !state.selectedId) return
  // Reveal the container before mounting so xterm/FitAddon measure real
  // dimensions rather than a display:none box.
  $('terminal-placeholder').classList.add('hidden')
  $('terminal').classList.remove('hidden')
  ensureTerm()
  const desiredPath = state.projectPath || null

  // Already attached to this VPS's shell. If the path context changed (e.g.
  // switched to another project on the same server, or back to a plain server),
  // re-root the live shell instead of leaving it in the old directory.
  if (term.vpsId === state.selectedId) {
    if (term.rootedPath !== desiredPath) {
      WriteShell(term.vpsId, desiredPath ? `cd ${shQuote(desiredPath)}\n` : `cd ~\n`).catch(() => {})
      term.rootedPath = desiredPath
    }
    fitTerm()
    term.inst.focus()
    return
  }

  await detachShell()
  const id = state.selectedId
  term.vpsId = id
  term.inst.reset()

  term.offOutput = EventsOn('shell:output:' + id, (data) => {
    term.inst.write(b64ToBytes(data))
  })
  term.offExit = EventsOn('shell:exit:' + id, () => {
    term.inst.write('\r\n\x1b[33m[session closed]\x1b[0m\r\n')
    if (term.vpsId === id) term.vpsId = null
  })

  try {
    term.fit.fit()
    const { cols, rows } = term.inst
    await StartShell(id, cols, rows)
    // When opened from a project, drop the shell into the deploy path.
    if (desiredPath) {
      WriteShell(id, `cd ${shQuote(desiredPath)}\n`).catch(() => {})
    }
    term.rootedPath = desiredPath
    term.inst.focus()
  } catch (e) {
    term.inst.write('\r\n\x1b[31mFailed to open shell: ' + errMsg(e) + '\x1b[0m\r\n')
    await detachShell()
  }
}

// Tear down event subscriptions and the remote shell for the attached VPS.
async function detachShell() {
  if (term.offOutput) { term.offOutput(); term.offOutput = null }
  if (term.offExit) { term.offExit(); term.offExit = null }
  const id = term.vpsId
  term.vpsId = null
  term.rootedPath = null
  if (id) {
    try { await CloseShell(id) } catch {}
  }
}

// Print arbitrary text into the terminal view (used by the container-logs
// shortcut). Normalizes line endings for the raw terminal.
function termPrint(text) {
  ensureTerm()
  $('terminal-placeholder').classList.add('hidden')
  $('terminal').classList.remove('hidden')
  term.inst.write(text.replace(/\r?\n/g, '\r\n'))
}

// ─────────── Ask (Codex CLI over MCP) ───────────
// The backend runs one Codex query at a time, global to the app (not keyed
// per VPS), and streams it through the "ai:log" (one JSON-encoded AIEvent
// per line) / "ai:done" (error string, "" on success) events. Those
// subscriptions are registered once at startup, below, since there's only
// ever one in-flight query to listen for.
const ask = {
  running: false,
  available: null, // resolved lazily, cached across tab visits
  openTools: new Map(), // tool-call DOM nodes still "in_progress", keyed by a synthetic index
  toolSeq: 0,
}

async function askOpen() {
  if (ask.available === null) {
    try { ask.available = await AskAIAvailable() } catch { ask.available = false }
  }
  $('ask-unavailable').classList.toggle('hidden', ask.available)
  $('ask-body').classList.toggle('hidden', !ask.available)
  askAutosize()
}

function askAutosize() {
  const ta = $('ask-input')
  ta.style.height = 'auto'
  ta.style.height = Math.min(ta.scrollHeight, 140) + 'px'
}

function askSetRunning(running) {
  ask.running = running
  $('ask-send').disabled = running
  $('ask-stop').classList.toggle('hidden', !running)
  $('ask-input').disabled = running
}

// askAppend adds one bubble to the transcript and scrolls it into view.
// kind: 'user' | 'message' | 'error' | 'status'.
function askAppend(kind, text) {
  const t = $('ask-transcript')
  const el = document.createElement('div')
  el.className = 'ask-entry ' + kind
  el.textContent = text
  t.appendChild(el)
  t.scrollTop = t.scrollHeight
  return el
}

// askAppendTool renders a tool call as a collapsible row (click to see its
// result/error) that starts amber/"in progress" and settles green/red.
function askAppendTool(ev) {
  const t = $('ask-transcript')
  const wrap = document.createElement('div')
  wrap.className = 'ask-tool ' + (ev.status || 'in_progress')
  wrap.innerHTML = `
    <div class="ask-tool-head">
      <span class="dot"></span>
      <span class="name mono"></span>
    </div>
    <div class="ask-tool-detail mono"></div>
  `
  wrap.querySelector('.name').textContent = ev.tool || 'tool'
  wrap.querySelector('.ask-tool-detail').textContent = ev.detail || ''
  wrap.querySelector('.ask-tool-head').addEventListener('click', () => wrap.classList.toggle('open'))
  t.appendChild(wrap)
  t.scrollTop = t.scrollHeight
  const key = ++ask.toolSeq
  ask.openTools.set(ev.tool + '\x00' + key, wrap)
  return wrap
}

// Tool calls arrive as two events (in_progress, then completed/failed) for
// the SAME call. Since AIEvent carries no call id, match the most recent
// still-"in_progress" row for that tool name — good enough for the common
// case (Codex mostly doesn't fire two concurrent calls to the identical
// tool), and worst case just settles the wrong-but-same-named row instead of
// leaving one stuck spinning forever.
function askSettleTool(ev) {
  for (const [key, node] of ask.openTools) {
    if (key.startsWith(ev.tool + '\x00') && node.classList.contains('in_progress')) {
      node.classList.remove('in_progress')
      node.classList.add(ev.status || 'completed')
      if (ev.detail) node.querySelector('.ask-tool-detail').textContent = ev.detail
      ask.openTools.delete(key)
      return
    }
  }
  // No matching in-progress row (shouldn't normally happen) — render fresh.
  askAppendTool(ev)
}

function askHandleEvent(raw) {
  let ev
  try { ev = JSON.parse(raw) } catch { return }
  switch (ev.kind) {
    case 'message':
      askAppend('message', ev.text)
      break
    case 'error':
      askAppend('error', '✗ ' + ev.text)
      break
    case 'tool_call':
      if (ev.status === 'in_progress') askAppendTool(ev)
      else askSettleTool(ev)
      break
  }
}

async function askSend() {
  if (ask.running || !state.selectedId) return
  const ta = $('ask-input')
  const prompt = ta.value.trim()
  if (!prompt) return
  askAppend('user', prompt)
  ta.value = ''
  askAutosize()
  ask.openTools.clear()
  askSetRunning(true)

  ask.logOff = EventsOn('ai:log', askHandleEvent)
  ask.doneOff = EventsOn('ai:done', (errStr) => {
    askSetRunning(false)
    if (errStr) askAppend('error', '✗ ' + errStr)
    if (ask.logOff) { ask.logOff(); ask.logOff = null }
    if (ask.doneOff) { ask.doneOff(); ask.doneOff = null }
  })
  try {
    await StartAIQuery(state.selectedId, prompt, $('ask-sudo').checked)
  } catch (e) {
    askAppend('error', '✗ ' + errMsg(e))
    askSetRunning(false)
    if (ask.logOff) { ask.logOff(); ask.logOff = null }
    if (ask.doneOff) { ask.doneOff(); ask.doneOff = null }
  }
}

async function askStop() {
  try { await StopAIQuery() } catch {}
}

// ─────────── Migration ───────────
// Three-stage wizard backed by a single inventory object held in memory:
// (1) pick source/target + paths → (2) inspect & review → (3) run + live log.
const mig = {
  inventory: null, // last Inspect result
  composeFile: '', // detected filename in source dir
  useSudo: false,  // captured at inspect time, applied at run time
  running: false,
  logOff: null,    // EventsOn cancel fn for migration:log
  doneOff: null,
}

function migShowStage(name) {
  for (const id of ['mig-setup', 'mig-substacks', 'mig-inventory', 'mig-run-view']) {
    $(id).classList.toggle('hidden', id !== 'mig-' + name && id !== name)
  }
}

function migPopulateVpsSelects() {
  // Migration is SSH-to-SSH only — the local machine can't be a source/target.
  const servers = state.vpses.filter((v) => !v.isLocal)
  const opts = servers.map((v) => `<option value="${v.id}">${v.name} (${v.user}@${v.host})</option>`).join('')
  for (const id of ['mig-src', 'mig-dst']) {
    const sel = $(id)
    const prev = sel.value
    sel.innerHTML = opts
    if (prev) sel.value = prev
  }
  // Sensible defaults: source = currently-selected VPS, target = first other.
  if (state.selectedId && !isLocalSel()) $('mig-src').value = state.selectedId
  const others = servers.filter((v) => v.id !== $('mig-src').value)
  if (others.length && !$('mig-dst').value) $('mig-dst').value = others[0].id
}

function migOpen() {
  migPopulateVpsSelects()
  migShowStage('setup')
  $('mig-setup-err').classList.add('hidden')
}

async function migInspect() {
  const srcId = $('mig-src').value
  const dstId = $('mig-dst').value
  const srcPath = $('mig-src-path').value.trim()
  const dstPath = $('mig-dst-path').value.trim() || srcPath
  const useSudo = $('mig-sudo').checked
  $('mig-dst-path').value = dstPath
  const err = (msg) => {
    const el = $('mig-setup-err')
    el.textContent = msg
    el.classList.remove('hidden')
  }
  if (!srcId || !dstId) return err('Pick both source and target VPSs.')
  if (srcId === dstId) return err('Source and target must be different VPSs.')
  if (!srcPath) return err('Enter the compose directory path on source.')

  // No password prompting: the SSH users are expected to have passwordless
  // sudo. If they don't, the command fails and the error is shown as-is.
  $('mig-setup-err').classList.add('hidden')
  const btn = $('mig-inspect')
  btn.disabled = true
  btn.textContent = 'Inspecting…'
  try {
    // Recover what the running stack actually uses (compose files + project name)
    // from its container labels — handles non-standard filenames, override files,
    // multi-`-f`, and custom -p.
    let cc = null
    try { cc = await DiscoverComposeContext(srcId, srcPath, useSudo) } catch { cc = null }
    const manualFile = $('mig-compose-file').value.trim()
    const projectName = $('mig-project-name').value.trim() || (cc && cc.project) || ''
    // composeFiles: [] means "let compose auto-detect (keeps override merge)";
    // a non-empty list forces exactly those -f files.
    // Priority: manual field > running stack's files > [] (a standard file exists).
    let composeFiles = null
    if (manualFile) composeFiles = [manualFile]
    else if (cc && cc.composeFiles && cc.composeFiles.length) composeFiles = cc.composeFiles
    else {
      let std = ''
      try { std = await FindComposeFile(srcId, srcPath) } catch { std = '' }
      if (std) composeFiles = [] // exists; auto-detect so default+override merge stays
    }
    if (composeFiles !== null) {
      const inv = await InspectMigration(srcId, srcPath, composeFiles, projectName, useSudo)
      mig.inventory = inv
      mig.composeFiles = composeFiles
      mig.projectName = projectName
      mig.useSudo = useSudo
      if (!$('mig-project-name').value.trim() && projectName) $('mig-project-name').value = projectName
      migRenderInventory(inv, srcPath, dstPath, composeFiles.length ? composeFiles.join(', ') : '(auto-detected)')
      migShowStage('inventory')
      return
    }
    // No compose file here — maybe it's a parent folder of several stacks.
    const subs = await DiscoverStacks(srcId, srcPath, useSudo) || []
    if (subs.length) {
      mig.useSudo = useSudo
      migRenderSubstacks(subs, srcPath, dstPath)
      migShowStage('substacks')
      return
    }
    err(`No compose file in ${srcPath}, and no stacks found in its subdirectories.`)
  } catch (e) {
    err('Inspect failed: ' + errMsg(e))
  } finally {
    btn.disabled = false
    btn.textContent = 'Inspect stack'
  }
}

function migRenderSubstacks(subs, srcRoot, dstRoot) {
  mig.substacks = subs
  mig.srcRoot = srcRoot
  mig.dstRoot = dstRoot
  $('mig-substack-target').textContent = dstRoot
  $('mig-substack-err').classList.add('hidden')
  $('mig-substack-list').innerHTML = subs.map((s) =>
    `<label class="checkbox-row"><input type="checkbox" class="mig-sub" value="${htmlEsc(s.name)}" checked /><span><code>${htmlEsc(s.name)}</code> <span class="muted">— ${htmlEsc(s.composeFile)}</span></span></label>`
  ).join('')
}

// Shared migration:log line handler — replaces an in-place progress line (zero-
// width-space prefixed) instead of appending, matching the Go progress marker.
const MIG_PROG = '​'
function migAppendLog(line) {
  const el = $('mig-log')
  let txt = el.textContent
  if (txt.endsWith('\n')) {
    const body = txt.slice(0, -1)
    const nl = body.lastIndexOf('\n')
    if (body.slice(nl + 1).startsWith(MIG_PROG)) txt = txt.slice(0, nl + 1)
  }
  el.textContent = txt + line + '\n'
  el.scrollTop = el.scrollHeight
}

async function migRunMulti() {
  if (mig.running) return
  const picked = Array.from(document.querySelectorAll('.mig-sub:checked')).map((c) => c.value)
  if (!picked.length) {
    const el = $('mig-substack-err'); el.textContent = 'Select at least one stack.'; el.classList.remove('hidden')
    return
  }
  const srcId = $('mig-src').value
  const dstId = $('mig-dst').value
  mig.running = true
  $('mig-outcome').classList.add('hidden')
  $('mig-reset').classList.add('hidden')
  $('mig-cleanup').classList.add('hidden')
  migShowStage('run-view')
  $('mig-log').textContent = mig.useSudo
    ? '⚡ Sudo: ON — remote commands prefixed with sudo on both VPSes.\n\n'
    : '⚡ Sudo: off — running as the SSH user on both VPSes.\n\n'
  mig.logOff = EventsOn('migration:log', migAppendLog)
  mig.doneOff = EventsOn('migration:done', (errStr) => {
    mig.running = false
    const out = $('mig-outcome')
    if (errStr) { out.textContent = '✗ ' + errStr; out.className = 'err' }
    else { out.textContent = '✓ All selected stacks migrated. Verify the target before shutting down source.'; out.className = 'ok' }
    out.classList.remove('hidden')
    $('mig-reset').classList.remove('hidden')
    if (mig.logOff) { mig.logOff(); mig.logOff = null }
    if (mig.doneOff) { mig.doneOff(); mig.doneOff = null }
  })
  try {
    await RunMultiMigration(srcId, dstId, mig.srcRoot, mig.dstRoot, picked, mig.useSudo)
  } catch (e) {
    mig.running = false
    const out = $('mig-outcome'); out.textContent = '✗ Failed to start: ' + errMsg(e); out.className = 'err'
    out.classList.remove('hidden'); $('mig-reset').classList.remove('hidden')
    if (mig.logOff) { mig.logOff(); mig.logOff = null }
    if (mig.doneOff) { mig.doneOff(); mig.doneOff = null }
  }
}

function migRenderInventory(inv, srcPath, dstPath, composeFile) {
  const fmtBytes = (n) => {
    if (!n) return '—'
    if (n < 1024) return n + ' B'
    if (n < 1024 ** 2) return (n / 1024).toFixed(1) + ' KB'
    if (n < 1024 ** 3) return (n / 1024 / 1024).toFixed(1) + ' MB'
    return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
  }
  const totalSize = (inv.volumes || []).reduce((s, v) => s + (v.size || 0), 0)

  const sections = []
  sections.push(`<h4>Project</h4><div><code>${inv.projectName}</code> · compose file <code>${composeFile}</code></div>`)
  sections.push(`<h4>Source → Target</h4><div class="mono" style="font-size:12px;"><code>${srcPath}</code> → <code>${dstPath}</code></div>`)
  const sudoLabel = mig.useSudo
    ? '<strong style="color:#86efac;">ON</strong> — all remote commands run via sudo'
    : '<strong style="color:#fca5a5;">off</strong> — commands run as the SSH user'
  sections.push(`<h4>Sudo</h4><div>${sudoLabel}</div>`)

  sections.push(`<h4>Services (${(inv.services || []).length})</h4>`)
  if ((inv.services || []).length) {
    sections.push('<ul>' + inv.services.map((s) =>
      `<li><strong>${s.name}</strong> — <code>${s.image || '(built)'}</code>${s.isPostgres ? ' <span class="muted">[postgres]</span>' : ''}${s.builds ? ' <span class="muted">[builds]</span>' : ''}</li>`
    ).join('') + '</ul>')
  } else {
    sections.push('<div class="muted">No services found.</div>')
  }

  sections.push(`<h4>Named volumes (${(inv.volumes || []).length}, total ${fmtBytes(totalSize)})</h4>`)
  if ((inv.volumes || []).length) {
    sections.push('<ul>' + inv.volumes.map((v) =>
      `<li><code>${v.name}</code> · ${fmtBytes(v.size)}</li>`
    ).join('') + '</ul>')
  } else {
    sections.push('<div class="muted">No named volumes — nothing to archive.</div>')
  }

  if ((inv.envFiles || []).length) {
    sections.push(`<h4>Env files (${inv.envFiles.length})</h4>`)
    sections.push('<ul>' + inv.envFiles.map((p) => `<li><code>${p}</code></li>`).join('') + '</ul>')
  }

  if ((inv.externalNetworks || []).length) {
    sections.push(`<h4>External networks (${inv.externalNetworks.length})</h4>`)
    sections.push('<ul>' + inv.externalNetworks.map((n) =>
      `<li><code>${n}</code> <span class="muted">— created on target if missing</span></li>`
    ).join('') + '</ul>')
  }

  if ((inv.buildImages || []).length) {
    sections.push(`<h4>Images copied from source (${inv.buildImages.length})</h4>`)
    sections.push('<ul>' + inv.buildImages.map((n) =>
      `<li><code>${n}</code> <span class="muted">— not in a registry; shipped via docker save/load so the target needn't pull or build it</span></li>`
    ).join('') + '</ul>')
  }

  if ((inv.bindMounts || []).length) {
    sections.push(`<div class="warn"><strong>Bind mounts not migrated:</strong><ul>` +
      inv.bindMounts.map((p) => `<li><code>${p}</code></li>`).join('') + '</ul>Copy these manually if they hold data.</div>')
  }
  for (const w of inv.warnings || []) {
    sections.push(`<div class="warn">${w}</div>`)
  }

  $('mig-summary').innerHTML = sections.join('')
}

async function migRun() {
  if (mig.running) return
  const srcId = $('mig-src').value
  const dstId = $('mig-dst').value
  const opts = {
    sourcePath: $('mig-src-path').value.trim(),
    targetPath: $('mig-dst-path').value.trim(),
    composeFiles: mig.composeFiles || [],
    projectName: mig.projectName || '',
    volumes: (mig.inventory.volumes || []).map((v) => v.name),
    envFiles: mig.inventory.envFiles || [],
    externalNetworks: mig.inventory.externalNetworks || [],
    buildImages: mig.inventory.buildImages || [],
  }

  mig.running = true
  $('mig-log').textContent = ''
  $('mig-outcome').classList.add('hidden')
  $('mig-reset').classList.add('hidden')
  $('mig-cleanup').classList.add('hidden')
  migShowStage('run-view')
  // First log line tells the user, in writing, whether sudo is actually in
  // effect for this run — so a forgotten checkbox can't quietly cause the
  // "mkdir: Permission denied" trap.
  const el = $('mig-log')
  el.textContent = mig.useSudo
    ? '⚡ Sudo: ON — remote commands prefixed with sudo on both VPSes.\n\n'
    : '⚡ Sudo: off — running as the SSH user on both VPSes.\n\n'

  // Subscribe before kicking off so early lines aren't missed. migAppendLog
  // handles in-place progress lines (zero-width-space prefixed).
  mig.logOff = EventsOn('migration:log', migAppendLog)
  mig.doneOff = EventsOn('migration:done', (errStr) => {
    mig.running = false
    const out = $('mig-outcome')
    if (errStr) {
      out.textContent = '✗ Migration failed: ' + errStr
      out.className = 'err'
    } else {
      out.textContent = '✓ Migration completed. Verify the target before shutting down source.'
      out.className = 'ok'
      // Offer a shortcut to tear the source stack down once verified.
      $('mig-cleanup').classList.remove('hidden')
    }
    out.classList.remove('hidden')
    $('mig-reset').classList.remove('hidden')
    if (mig.logOff) { mig.logOff(); mig.logOff = null }
    if (mig.doneOff) { mig.doneOff(); mig.doneOff = null }
  })

  try {
    await RunMigration(srcId, dstId, opts, mig.useSudo)
  } catch (e) {
    // Synchronous failure (couldn't even start)
    mig.running = false
    const out = $('mig-outcome')
    out.textContent = '✗ Failed to start: ' + errMsg(e)
    out.className = 'err'
    out.classList.remove('hidden')
    $('mig-reset').classList.remove('hidden')
    if (mig.logOff) { mig.logOff(); mig.logOff = null }
    if (mig.doneOff) { mig.doneOff(); mig.doneOff = null }
  }
}

// ─────────── Cleanup / teardown ───────────
const cleanup = { inventory: null, running: false, logOff: null, doneOff: null, project: '', composeFiles: [] }

const htmlEsc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]))

function cleanupShowStage(name) {
  for (const id of ['cleanup-setup', 'cleanup-review', 'cleanup-run-view']) {
    $(id).classList.toggle('hidden', id !== 'cleanup-' + name && id !== name)
  }
}

function cleanupOpen() {
  const err = $('cleanup-setup-err')
  err.classList.add('hidden')
  if (isLocalSel()) {
    cleanupShowStage('setup')
    err.textContent = 'Cleanup runs on a remote VPS.'
    err.classList.remove('hidden')
    return
  }
  cleanupShowStage('setup')
  if (state.projectPath && !$('cleanup-path').value) $('cleanup-path').value = state.projectPath
}

async function cleanupInspect() {
  const path = $('cleanup-path').value.trim()
  const useSudo = $('cleanup-sudo').checked
  const fail = (m) => { const el = $('cleanup-setup-err'); el.textContent = m; el.classList.remove('hidden') }
  if (isLocalSel()) return fail('Cleanup runs on a remote VPS.')
  if (!state.selectedId) return fail('Select a VPS first.')
  if (!path) return fail('Enter the compose directory.')
  $('cleanup-setup-err').classList.add('hidden')
  const btn = $('cleanup-inspect'); btn.disabled = true; btn.textContent = 'Inspecting…'
  try {
    // Recover the real project/compose files so `down` targets the right project.
    let cc = null
    try { cc = await DiscoverComposeContext(state.selectedId, path, useSudo) } catch { cc = null }
    cleanup.project = (cc && cc.project) || ''
    cleanup.composeFiles = (cc && cc.composeFiles) || []
    const inv = await InspectTeardown(state.selectedId, path, cleanup.composeFiles, cleanup.project, useSudo)
    cleanup.inventory = inv
    cleanupRenderReview(inv, path)
    cleanupShowStage('review')
  } catch (e) {
    fail('Inspect failed: ' + errMsg(e))
  } finally {
    btn.disabled = false; btn.textContent = 'Inspect stack'
  }
}

function cleanupRenderReview(inv, path) {
  const base = path.replace(/\/+$/, '').split('/').pop() || path
  $('cleanup-confirm-name').textContent = base
  $('cleanup-confirm').value = ''
  $('cleanup-run').disabled = true
  const sec = []
  sec.push(`<h4>Stack</h4><div class="mono" style="font-size:12px;"><code>${htmlEsc(path)}</code></div>`)
  sec.push(`<h4>Services (${(inv.services || []).length})</h4>`)
  if ((inv.services || []).length) {
    sec.push('<ul>' + inv.services.map((s) => `<li><strong>${htmlEsc(s.name)}</strong> — <code>${htmlEsc(s.image || '(built)')}</code></li>`).join('') + '</ul>')
  } else sec.push('<div class="muted">none</div>')
  sec.push(`<h4>Named volumes (${(inv.volumes || []).length})</h4>`)
  if ((inv.volumes || []).length) {
    sec.push('<ul>' + inv.volumes.map((v) => `<li><code>${htmlEsc(v.name)}</code></li>`).join('') + '</ul>')
  } else sec.push('<div class="muted">none</div>')
  if ((inv.externalNetworks || []).length) {
    sec.push('<h4>External networks (left untouched)</h4>')
    sec.push('<ul>' + inv.externalNetworks.map((n) => `<li><code>${htmlEsc(n)}</code></li>`).join('') + '</ul>')
  }
  $('cleanup-summary').innerHTML = sec.join('')
}

async function cleanupRun() {
  if (cleanup.running) return
  const opts = {
    path: $('cleanup-path').value.trim(),
    project: cleanup.project || '',
    composeFiles: cleanup.composeFiles || [],
    removeVolumes: $('cleanup-vols').checked,
    removeImages: $('cleanup-images').checked,
    removeDir: $('cleanup-dir').checked,
  }
  cleanup.running = true
  $('cleanup-log').textContent = ''
  $('cleanup-outcome').classList.add('hidden')
  $('cleanup-reset').classList.add('hidden')
  cleanupShowStage('run-view')
  cleanup.logOff = EventsOn('cleanup:log', (line) => {
    const el = $('cleanup-log'); el.textContent += line + '\n'; el.scrollTop = el.scrollHeight
  })
  cleanup.doneOff = EventsOn('cleanup:done', (errStr) => {
    cleanup.running = false
    const out = $('cleanup-outcome')
    if (errStr) { out.textContent = '✗ Cleanup failed: ' + errStr; out.className = 'err' }
    else { out.textContent = '✓ Stack removed.'; out.className = 'ok' }
    out.classList.remove('hidden')
    $('cleanup-reset').classList.remove('hidden')
    if (cleanup.logOff) { cleanup.logOff(); cleanup.logOff = null }
    if (cleanup.doneOff) { cleanup.doneOff(); cleanup.doneOff = null }
  })
  try {
    await TeardownStack(state.selectedId, opts, $('cleanup-sudo').checked)
  } catch (e) {
    cleanup.running = false
    const out = $('cleanup-outcome'); out.textContent = '✗ Failed to start: ' + errMsg(e); out.className = 'err'
    out.classList.remove('hidden'); $('cleanup-reset').classList.remove('hidden')
    if (cleanup.logOff) { cleanup.logOff(); cleanup.logOff = null }
    if (cleanup.doneOff) { cleanup.doneOff(); cleanup.doneOff = null }
  }
}

// ─────────── Backups ───────────
const bak = { jobs: [], editing: null, dbItems: [], runOff: null, doneOff: null }

function bakShowView(name) {
  const ids = ['backups-list-view', 'backups-form-view', 'backups-run-view', 'backups-snap-view',
    'backups-restore-view', 'backups-restore-run']
  for (const id of ids) {
    $(id).classList.toggle('hidden', id !== 'backups-' + name && id !== name)
  }
}
const bakSudo = () => $('backups-sudo').checked

async function backupsOpen() {
  bakShowView('list-view')
  await backupsRefresh()
}

async function backupsRefresh() {
  const list = $('backups-list')
  if (isLocalSel()) { list.innerHTML = '<p class="muted center-msg">Backups run on a remote VPS.</p>'; return }
  if (!state.selectedId) { list.innerHTML = '<p class="muted center-msg">Select a VPS.</p>'; return }
  list.innerHTML = '<p class="muted center-msg">Loading…</p>'
  try {
    bak.jobs = await ListBackupJobs(state.selectedId, bakSudo()) || []
    backupsRenderList()
  } catch (e) {
    list.innerHTML = `<p class="muted center-msg">Error: ${htmlEsc(errMsg(e))}</p>`
  }
}

function backupsRenderList() {
  const list = $('backups-list')
  if (!bak.jobs.length) {
    list.innerHTML = '<p class="muted center-msg">No backup jobs on this VPS yet. Create one with “+ New backup”.</p>'
    return
  }
  list.innerHTML = bak.jobs.map((j) => {
    const when = j.lastRun ? ' · ' + new Date(j.lastRun).toLocaleString() : ''
    const status = j.lastStatus ? `<span class="state">${htmlEsc(j.lastStatus)}${htmlEsc(when)}</span>` : ''
    const dest = `${j.bucket?.bucket || '?'}${j.bucket?.prefix ? '/' + j.bucket.prefix : ''}`
    const dis = j.enabled ? '' : ' <span class="muted">(disabled)</span>'
    return `<div class="container-row">
      <div class="info">
        <div class="name-line"><span class="name">${htmlEsc(j.name)}</span>${status}</div>
        <div class="meta mono">${htmlEsc(j.schedule)} · ${htmlEsc(dest)}${dis}</div>
      </div>
      <div class="actions">
        <button data-act="run" data-id="${j.id}" class="btn btn-secondary">Run now</button>
        <button data-act="snap" data-id="${j.id}" class="btn btn-secondary">Snapshots</button>
        <button data-act="edit" data-id="${j.id}" class="btn btn-secondary">Edit</button>
        <button data-act="del" data-id="${j.id}" class="btn btn-danger">Delete</button>
      </div>
    </div>`
  }).join('')
  list.querySelectorAll('button[data-act]').forEach((b) =>
    b.addEventListener('click', () => bakAction(b.dataset.act, b.dataset.id)))
}

function bakAction(act, id) {
  const job = bak.jobs.find((j) => j.id === id)
  if (act === 'run') return bakRunNow(id, job)
  if (act === 'snap') return bakSnapshots(id, job)
  if (act === 'edit') return bakOpenForm(job)
  if (act === 'del') return bakDelete(id, job)
}

function bakOpenForm(job) {
  bak.editing = job || null
  $('backups-form-title').textContent = job ? 'Edit backup' : 'New backup'
  $('bak-id').value = job?.id || ''
  $('bak-name').value = job?.name || ''
  $('bak-path').value = job?.stackPath || state.projectPath || ''
  $('bak-compose').checked = job ? !!job.includeCompose : true
  $('bak-schedule').value = job?.schedule || '0 2 * * *'
  $('bak-keep-daily').value = job?.retention?.keepDaily ?? 7
  $('bak-keep-weekly').value = job?.retention?.keepWeekly ?? 4
  $('bak-keep-monthly').value = job?.retention?.keepMonthly ?? 6
  $('bak-endpoint').value = job?.bucket?.endpoint || ''
  $('bak-bucket').value = job?.bucket?.bucket || ''
  $('bak-region').value = job?.bucket?.region || ''
  $('bak-prefix').value = job?.bucket?.prefix || ''
  const keepHint = job ? 'leave blank to keep' : ''
  for (const f of ['bak-access', 'bak-secret', 'bak-restic-pw']) { $(f).value = ''; $(f).placeholder = keepHint }
  $('bak-enabled').checked = job ? !!job.enabled : true
  $('bak-form-err').classList.add('hidden')
  $('bak-detect-note').textContent = ''
  bakRenderVolumes((job?.volumes || []).map((v) => ({ name: v, checked: true })))
  bakRenderDatabases((job?.databases || []).map((d) => ({ ...d, checked: true })))
  const prov = bakProviderFromEndpoint(job?.bucket?.endpoint)
  $('bak-provider').value = prov
  bakApplyProvider(prov)
  bakShowView('form-view')
}

// bakProviderFromEndpoint guesses the provider for the preset dropdown. New jobs
// (no endpoint) default to R2 since that's the most-asked target.
function bakProviderFromEndpoint(ep) {
  const e = (ep || '').toLowerCase()
  if (e.includes('r2.cloudflarestorage.com')) return 'r2'
  if (e.includes('backblazeb2.com')) return 'b2'
  if (e.includes('amazonaws.com')) return 'aws'
  return e ? 'other' : 'r2'
}

// bakApplyProvider prefills the endpoint/region hints for the chosen provider so
// each one works out of the box — notably Cloudflare R2 (region "auto", which
// restic needs and most people don't know).
function bakApplyProvider(p) {
  const ep = $('bak-endpoint'); const region = $('bak-region'); const hint = $('bak-provider-hint')
  if (p === 'r2') {
    ep.placeholder = 'https://<ACCOUNT_ID>.r2.cloudflarestorage.com'
    if (!region.value || region.value === 'us-east-1' || region.value === 'us-west-002') region.value = 'auto'
    hint.innerHTML = 'Create an <strong>R2 API token</strong> (Cloudflare → R2 → Manage API Tokens → <em>Object Read &amp; Write</em>) and use its Access Key ID + Secret below. The endpoint is your account’s S3 URL; region is <code>auto</code>.'
  } else if (p === 'b2') {
    ep.placeholder = 's3.us-west-002.backblazeb2.com'
    if (region.value === 'auto') region.value = ''
    region.placeholder = 'us-west-002'
    hint.textContent = 'Use a Backblaze application key (keyID + applicationKey). The region matches your endpoint, e.g. us-west-002.'
  } else if (p === 'aws') {
    ep.placeholder = 's3.amazonaws.com'
    if (region.value === 'auto') region.value = ''
    region.placeholder = 'us-east-1'
    hint.textContent = 'Use an IAM access key with read/write on the bucket. Set the bucket’s region.'
  } else {
    ep.placeholder = 'https://s3.example.com'
    hint.textContent = 'Any S3-compatible endpoint — include https:// in the endpoint URL.'
  }
}

function bakRenderVolumes(items) {
  const el = $('bak-volumes')
  if (!items.length) { el.innerHTML = '<span class="muted">No volumes detected. Enter the stack dir and click Detect.</span>'; return }
  el.innerHTML = '<h4 style="margin:0 0 6px;">Volumes</h4>' + items.map((v) =>
    `<label class="checkbox-row"><input type="checkbox" class="bak-vol" value="${htmlEsc(v.name)}" ${v.checked !== false ? 'checked' : ''}/><span><code>${htmlEsc(v.name)}</code></span></label>`
  ).join('')
}

function bakRenderDatabases(items) {
  bak.dbItems = items
  const el = $('bak-databases')
  if (!items.length) { el.innerHTML = '<span class="muted">No databases detected.</span>'; return }
  el.innerHTML = '<h4 style="margin:0 0 6px;">Databases</h4>' + items.map((d, i) =>
    `<label class="checkbox-row"><input type="checkbox" class="bak-db" data-i="${i}" ${d.checked !== false ? 'checked' : ''}/><span><code>${htmlEsc(d.container)}</code> · ${htmlEsc(d.engine)} · db <code>${htmlEsc(d.db || '?')}</code></span></label>`
  ).join('')
}

async function bakDetect() {
  const path = $('bak-path').value.trim()
  if (!state.selectedId || !path) { $('bak-detect-note').textContent = 'Enter the stack directory first.'; return }
  const btn = $('bak-detect'); btn.disabled = true; btn.textContent = 'Detecting…'
  try {
    const inv = await InspectTeardown(state.selectedId, path, bakSudo())
    bakRenderVolumes((inv.volumes || []).map((v) => ({ name: v.name, checked: true })))
    const dbc = await ListDBContainers(state.selectedId, bakSudo(), path) || []
    bakRenderDatabases(dbc.map((c) => ({ container: c.name, engine: c.engine, user: c.user, db: c.defaultDb, checked: true })))
    $('bak-detect-note').textContent = `${(inv.volumes || []).length} volume(s), ${dbc.length} database(s)`
  } catch (e) {
    $('bak-detect-note').textContent = 'Detect failed: ' + errMsg(e)
  } finally {
    btn.disabled = false; btn.textContent = 'Detect volumes & databases'
  }
}

async function bakSave() {
  const fail = (m) => { const el = $('bak-form-err'); el.textContent = m; el.classList.remove('hidden') }
  const name = $('bak-name').value.trim()
  const path = $('bak-path').value.trim()
  const endpoint = $('bak-endpoint').value.trim()
  const bucket = $('bak-bucket').value.trim()
  if (!name) return fail('Enter a name.')
  if (!path) return fail('Enter the stack directory.')
  if (!endpoint || !bucket) return fail('Enter the bucket endpoint and name.')
  const volumes = Array.from(document.querySelectorAll('.bak-vol:checked')).map((c) => c.value)
  const databases = Array.from(document.querySelectorAll('.bak-db:checked'))
    .map((c) => bak.dbItems[parseInt(c.dataset.i, 10)]).filter(Boolean)
    .map((d) => ({ container: d.container, engine: d.engine, user: d.user, db: d.db }))
  const job = {
    id: $('bak-id').value || '',
    name, stackPath: path, volumes, databases,
    includeCompose: $('bak-compose').checked,
    schedule: $('bak-schedule').value.trim(),
    retention: {
      keepDaily: parseInt($('bak-keep-daily').value || '0', 10),
      keepWeekly: parseInt($('bak-keep-weekly').value || '0', 10),
      keepMonthly: parseInt($('bak-keep-monthly').value || '0', 10),
      keepLast: 0,
    },
    bucket: { endpoint, region: $('bak-region').value.trim(), bucket, prefix: $('bak-prefix').value.trim() },
    enabled: $('bak-enabled').checked,
  }
  const secrets = {
    accessKey: $('bak-access').value,
    secretKey: $('bak-secret').value,
    resticPassword: $('bak-restic-pw').value,
  }
  const hasSecret = secrets.accessKey || secrets.secretKey || secrets.resticPassword
  if (!bak.editing && (!secrets.accessKey || !secrets.secretKey || !secrets.resticPassword)) {
    return fail('Enter the access key, secret key and restic password.')
  }
  $('bak-form-err').classList.add('hidden')
  const btn = $('bak-save'); btn.disabled = true; btn.textContent = 'Testing & saving…'
  try {
    if (!bak.editing || hasSecret) {
      await TestBackupTarget(state.selectedId, job.bucket, secrets, bakSudo())
    }
    await SaveBackupJob(state.selectedId, job, secrets, bakSudo())
    bakShowView('list-view')
    await backupsRefresh()
  } catch (e) {
    fail('Save failed: ' + errMsg(e))
  } finally {
    btn.disabled = false; btn.textContent = 'Test & Save'
  }
}

function bakRunNow(id, job) {
  $('backups-run-title').textContent = 'Backing up: ' + (job?.name || id)
  $('backups-log').textContent = ''
  $('backups-outcome').classList.add('hidden')
  $('backups-run-back').classList.add('hidden')
  bakShowView('run-view')
  const finish = (out, cls) => {
    const o = $('backups-outcome'); o.textContent = out; o.className = cls
    o.classList.remove('hidden'); $('backups-run-back').classList.remove('hidden')
    if (bak.runOff) { bak.runOff(); bak.runOff = null }
    if (bak.doneOff) { bak.doneOff(); bak.doneOff = null }
  }
  bak.runOff = EventsOn('backup:log:' + id, (line) => {
    const el = $('backups-log'); el.textContent += line + '\n'; el.scrollTop = el.scrollHeight
  })
  bak.doneOff = EventsOn('backup:done:' + id, (errStr) => {
    if (errStr) finish('✗ Backup failed: ' + errStr, 'err')
    else { finish('✓ Backup complete.', 'ok'); backupsRefresh() }
  })
  RunBackupNow(state.selectedId, id, bakSudo()).catch((e) => finish('✗ Failed to start: ' + errMsg(e), 'err'))
}

async function bakSnapshots(id, job) {
  $('backups-snap-title').textContent = 'Snapshots: ' + (job?.name || id)
  const list = $('backups-snap-list')
  list.innerHTML = '<p class="muted center-msg">Loading…</p>'
  bakShowView('snap-view')
  try {
    const snaps = await ListBackupSnapshots(state.selectedId, id, bakSudo()) || []
    if (!snaps.length) { list.innerHTML = '<p class="muted center-msg">No snapshots yet.</p>'; return }
    list.innerHTML = snaps.map((s) => `<div class="container-row">
      <div class="info">
        <div class="name-line"><span class="name mono">${htmlEsc(s.id)}</span><span class="state">${htmlEsc(new Date(s.time).toLocaleString())}</span></div>
        <div class="meta mono">${htmlEsc((s.paths || []).join(', '))}</div>
      </div>
      <div class="actions">
        <button data-restore="${htmlEsc(s.id)}" class="btn btn-secondary">Restore</button>
        <button data-snap="${htmlEsc(s.id)}" class="btn btn-danger">Delete</button>
      </div>
    </div>`).join('')
    list.querySelectorAll('button[data-restore]').forEach((b) => b.addEventListener('click', () => bakOpenRestore(job, b.dataset.restore)))
    list.querySelectorAll('button[data-snap]').forEach((b) => b.addEventListener('click', () => bakForget(id, b.dataset.snap)))
  } catch (e) {
    list.innerHTML = `<p class="muted center-msg">Error: ${htmlEsc(errMsg(e))}</p>`
  }
}

async function bakForget(jobId, snapId) {
  if (!confirm('Delete snapshot ' + snapId + '? This prunes it from the bucket.')) return
  try {
    await ForgetBackupSnapshot(state.selectedId, jobId, snapId, bakSudo())
    bakSnapshots(jobId, bak.jobs.find((j) => j.id === jobId))
  } catch (e) { alert('Delete failed: ' + errMsg(e)) }
}

async function bakDelete(id, job) {
  if (!confirm(`Delete backup job "${job?.name || id}"? The cron schedule and config are removed; the bucket data is kept.`)) return
  try { await DeleteBackupJob(state.selectedId, id, bakSudo()); await backupsRefresh() }
  catch (e) { alert('Delete failed: ' + errMsg(e)) }
}

// ─────────── Restore ───────────
function bakOpenRestore(job, snapId) {
  bak.restoreJob = job
  bak.restoreSnap = snapId
  $('bak-restore-meta').innerHTML =
    `<div>Snapshot <code class="mono">${htmlEsc(snapId)}</code> of <strong>${htmlEsc(job.name)}</strong></div>` +
    `<div class="muted" style="font-size:12px;">bucket <code>${htmlEsc(job.bucket?.bucket || '')}${job.bucket?.prefix ? '/' + htmlEsc(job.bucket.prefix) : ''}</code></div>`
  const sel = $('bak-restore-vps')
  sel.innerHTML = state.vpses.filter((v) => !v.isLocal)
    .map((v) => `<option value="${v.id}">${htmlEsc(v.name)} (${htmlEsc(v.user)}@${htmlEsc(v.host)})</option>`).join('')
  sel.value = state.selectedId
  $('bak-restore-path').value = job.stackPath || ''
  $('bak-restore-vols').checked = true
  $('bak-restore-compose').checked = true
  $('bak-restore-up').checked = true
  $('bak-restore-import').checked = false
  for (const f of ['bak-restore-access', 'bak-restore-secret', 'bak-restore-pw']) $(f).value = ''
  $('bak-restore-sudo').checked = bakSudo()
  $('bak-restore-confirm').checked = false
  $('bak-restore-go').disabled = true
  $('bak-restore-err').classList.add('hidden')
  bakRestoreToggleCreds()
  bakShowView('restore-view')
}

// The origin VPS (the one whose backups we're viewing) already stores the
// credentials, so they're only needed when restoring somewhere else.
function bakRestoreToggleCreds() {
  $('bak-restore-creds').classList.toggle('hidden', $('bak-restore-vps').value === state.selectedId)
}

async function bakDoRestore() {
  const job = bak.restoreJob
  const targetVps = $('bak-restore-vps').value
  const sameVps = targetVps === state.selectedId
  const fail = (m) => { const el = $('bak-restore-err'); el.textContent = m; el.classList.remove('hidden') }
  const opts = {
    snapshotId: bak.restoreSnap,
    targetPath: $('bak-restore-path').value.trim(),
    restoreVolumes: $('bak-restore-vols').checked,
    restoreCompose: $('bak-restore-compose').checked,
    composeUp: $('bak-restore-up').checked,
    importDatabases: $('bak-restore-import').checked,
  }
  const secrets = sameVps
    ? { accessKey: '', secretKey: '', resticPassword: '' }
    : { accessKey: $('bak-restore-access').value, secretKey: $('bak-restore-secret').value, resticPassword: $('bak-restore-pw').value }
  if (!sameVps && (!secrets.accessKey || !secrets.secretKey || !secrets.resticPassword)) {
    return fail('Restoring on another VPS needs the access key, secret and restic password.')
  }
  if ((opts.composeUp || opts.restoreCompose) && !opts.targetPath) {
    return fail('Enter the target compose directory (needed to restore compose files or start the stack).')
  }
  $('bak-restore-err').classList.add('hidden')
  $('bak-restore-log').textContent = ''
  $('bak-restore-outcome').classList.add('hidden')
  $('bak-restore-runback').classList.add('hidden')
  bakShowView('restore-run')
  const finish = (out, cls) => {
    const o = $('bak-restore-outcome'); o.textContent = out; o.className = cls
    o.classList.remove('hidden'); $('bak-restore-runback').classList.remove('hidden')
    if (bak.restoreOff) { bak.restoreOff(); bak.restoreOff = null }
    if (bak.restoreDoneOff) { bak.restoreDoneOff(); bak.restoreDoneOff = null }
  }
  bak.restoreOff = EventsOn('restore:log', (line) => {
    const el = $('bak-restore-log'); el.textContent += line + '\n'; el.scrollTop = el.scrollHeight
  })
  bak.restoreDoneOff = EventsOn('restore:done', (errStr) => {
    if (errStr) finish('✗ Restore failed: ' + errStr, 'err')
    else finish('✓ Restore complete.', 'ok')
  })
  RestoreBackup(targetVps, job, secrets, opts, $('bak-restore-sudo').checked)
    .catch((e) => finish('✗ Failed to start: ' + errMsg(e), 'err'))
}

// ─────────── Maintenance (disk / docker usage + prune) ───────────
const maint = { pruneOff: null }
const maintSudo = () => $('maint-sudo').checked

async function maintenanceOpen() {
  if (!state.selectedId) {
    $('maint-disk').innerHTML = '<span class="muted">Select a server first.</span>'
    $('maint-docker').textContent = ''
    $('maint-images').textContent = ''
    return
  }
  maintPruneToggle(false)
  await maintRefresh()
}

async function maintRefresh() {
  $('maint-disk').innerHTML = '<span class="muted">Loading…</span>'
  $('maint-docker').textContent = ''
  try {
    const u = await SystemUsage(state.selectedId, maintSudo())
    maintRenderDisk(u.disk)
    maintRenderDocker(u.docker)
  } catch (e) {
    $('maint-disk').innerHTML = '<span class="muted">Failed: ' + esc(errMsg(e)) + '</span>'
    $('maint-docker').textContent = ''
  }
  maintLoadImages()
}

function maintRenderDisk(d) {
  const el = $('maint-disk')
  if (!d || !d.available) {
    el.innerHTML = '<span class="muted">Couldn’t read disk usage.</span>'
    return
  }
  const pct = d.usePercent
  const cls = pct >= 90 ? ' danger' : pct >= 75 ? ' warn-bar' : ''
  el.innerHTML = `
    <div class="card-title">Disk — ${esc(d.mountPoint)} <span class="muted">(${esc(d.filesystem)})</span></div>
    <div class="usage-bar"><div class="usage-fill${cls}" style="width:${pct}%"></div></div>
    <div class="muted" style="font-size:12px;margin-top:4px;">${esc(d.usedHuman)} of ${esc(d.totalHuman)} used · ${esc(d.availHuman)} free · ${pct}%</div>`
}

function maintRenderDocker(dk) {
  const el = $('maint-docker')
  if (!dk || !dk.available) {
    el.innerHTML = '<span class="muted">Docker disk usage unavailable.</span>'
    return
  }
  const row = (c) =>
    `<tr><td>${esc(c.type)}</td><td>${c.totalCount} <span class="muted">(${c.activeCount} active)</span></td><td>${esc(c.sizeHuman)}</td><td>${esc(c.reclaimableHuman)}</td></tr>`
  el.innerHTML = `
    <div class="card-title">Docker</div>
    <table class="mini-table"><thead><tr><th></th><th>count</th><th>size</th><th>reclaimable</th></tr></thead>
    <tbody>${row(dk.images)}${row(dk.containers)}${row(dk.volumes)}${row(dk.buildCache)}</tbody></table>
    <div class="muted" style="font-size:12px;margin-top:6px;">Total ${esc(dk.totalHuman)} · ${esc(dk.reclaimableHuman)} reclaimable</div>`
}

async function maintLoadImages() {
  const el = $('maint-images')
  el.innerHTML = '<span class="muted">Loading…</span>'
  try {
    const imgs = await SystemLargestImages(state.selectedId, 10, maintSudo())
    if (!imgs || !imgs.length) {
      el.innerHTML = '<span class="muted">No images.</span>'
      return
    }
    el.innerHTML = imgs
      .map((i) => `<div class="img-row"><span class="mono">${esc(i.repository)}:${esc(i.tag)}</span><span class="muted">${esc(i.sizeHuman)}</span></div>`)
      .join('')
  } catch {
    el.innerHTML = '<span class="muted">Couldn’t list images.</span>'
  }
}

function maintPruneToggle(show) {
  $('maint-prune-view').classList.toggle('hidden', !show)
  if (!show) $('maint-prune-log').classList.add('hidden')
}

async function maintRunPrune() {
  const opts = { allImages: $('maint-prune-images').checked, volumes: $('maint-prune-volumes').checked }
  if (opts.volumes && !confirm('Removing unused volumes permanently deletes their data. Continue?')) return
  const log = $('maint-prune-log')
  log.classList.remove('hidden')
  log.textContent = ''
  $('maint-prune-run').disabled = true
  if (maint.pruneOff) { maint.pruneOff(); maint.pruneOff = null }
  maint.pruneOff = EventsOn('maintenance-prune', (line) => {
    log.textContent += line + '\n'
    log.scrollTop = log.scrollHeight
  })
  try {
    await PruneDocker(state.selectedId, opts, maintSudo())
    log.textContent += '\n✓ Done.\n'
    await maintRefresh()
  } catch (e) {
    log.textContent += '\n✗ ' + errMsg(e) + '\n'
  } finally {
    if (maint.pruneOff) { maint.pruneOff(); maint.pruneOff = null }
    $('maint-prune-run').disabled = false
  }
}

// ─────────── Provision a database ───────────
const prov = { engines: [], runOff: null }

async function provisionOpen() {
  $('prov-form-view').classList.remove('hidden')
  $('prov-run-view').classList.add('hidden')
  if (!prov.engines.length) {
    try {
      prov.engines = (await ProvisionDatabaseEngines()) || []
    } catch {
      prov.engines = []
    }
    $('prov-engine').innerHTML = prov.engines
      .map((e, i) => `<option value="${i}">${esc(e.label)}</option>`)
      .join('')
    $('prov-engine').onchange = provEngineChange
  }
  provEngineChange()
}

function provCurrentEngine() {
  return prov.engines[parseInt($('prov-engine').value || '0', 10)] || null
}

function provEngineChange() {
  const e = provCurrentEngine()
  if (!e) return
  $('prov-tag').innerHTML = (e.tags || []).map((t) => `<option value="${esc(t)}">${esc(t)}</option>`).join('')
  $('prov-user-field').classList.toggle('hidden', !e.needsUser)
  $('prov-db-field').classList.toggle('hidden', !e.needsDB)
  $('prov-pass-field').classList.toggle('hidden', !e.needsAuth)
  if (e.needsUser && !$('prov-user').value) $('prov-user').value = e.defaultUser || ''
}

async function provisionRun() {
  const e = provCurrentEngine()
  if (!e) return
  const name = $('prov-name').value.trim()
  const dir = $('prov-dir').value.trim()
  const err = $('prov-err')
  err.classList.add('hidden')
  if (!/^[A-Za-z][A-Za-z0-9_-]{0,62}$/.test(name)) {
    err.textContent = 'Name must start with a letter and use only letters, digits, - or _.'
    err.classList.remove('hidden')
    return
  }
  if (!dir) {
    err.textContent = 'Enter a target directory on the VPS.'
    err.classList.remove('hidden')
    return
  }
  const spec = {
    engine: e.kind,
    dir,
    name,
    tag: $('prov-tag').value,
    user: e.needsUser ? $('prov-user').value.trim() : '',
    database: e.needsDB ? $('prov-db').value.trim() : '',
    password: e.needsAuth ? $('prov-pass').value : '',
    exposePort: parseInt($('prov-port').value || '0', 10) || 0,
    volumeName: '',
  }
  $('prov-form-view').classList.add('hidden')
  $('prov-run-view').classList.remove('hidden')
  const log = $('prov-log')
  log.textContent = ''
  $('prov-result').classList.add('hidden')
  $('prov-reset').classList.add('hidden')
  if (prov.runOff) { prov.runOff(); prov.runOff = null }
  prov.runOff = EventsOn('provision-db', (line) => {
    log.textContent += line + '\n'
    log.scrollTop = log.scrollHeight
  })
  try {
    const res = await ProvisionDatabase(state.selectedId, spec, $('prov-sudo').checked)
    log.textContent += '\n✓ Database is up.\n'
    const r = $('prov-result')
    r.classList.remove('hidden')
    r.innerHTML = `
      <div class="result-card">
        <div class="card-title">Save the password now — it won't be shown again</div>
        <div class="kv"><span>Container</span><code>${esc(res.containerName)}</code></div>
        <div class="kv"><span>Image</span><code>${esc(res.image)}</code></div>
        <div class="kv"><span>Volume</span><code>${esc(res.volumeName)}</code></div>
        <div class="kv"><span>Password</span><code>${esc(res.password)}</code></div>
        ${res.backupEngine ? '<p class="muted" style="font-size:12px;margin:8px 0 0;">Add this container to a Backup job (engine: ' + esc(res.backupEngine) + ') to schedule dumps.</p>' : ''}
      </div>`
  } catch (e2) {
    log.textContent += '\n✗ ' + errMsg(e2) + '\n'
  } finally {
    if (prov.runOff) { prov.runOff(); prov.runOff = null }
    $('prov-reset').classList.remove('hidden')
  }
}

// ─────────── Domains (expose via caddy-docker-proxy + Cloudflare DNS) ───────────
const dom = { exposeOff: null }

function domainsOpen() {
  $('dom-form-view').classList.remove('hidden')
  $('dom-run-view').classList.add('hidden')
}

async function domDetect() {
  const card = $('dom-proxy')
  if (!state.selectedId) { card.textContent = 'Select a server first.'; return }
  card.className = 'info-card muted'
  card.textContent = 'Detecting…'
  try {
    const p = await DetectCaddyProxy(state.selectedId, $('dom-sudo').checked)
    if (p && p.present) {
      card.className = 'info-card ok'
      card.innerHTML =
        `caddy-docker-proxy detected: <code>${esc(p.container)}</code> <span class="muted">(${esc(p.image)})</span>` +
        (p.network ? `<br><span class="muted">routing network: ${esc(p.network)}</span>` : '')
      if (p.network && !$('dom-network').value) $('dom-network').value = p.network
    } else {
      card.className = 'info-card warn-card'
      card.innerHTML =
        'No caddy-docker-proxy found on this host — the label path won’t route until one is running. You can still set up DNS below.'
    }
  } catch (e) {
    card.className = 'info-card'
    card.textContent = 'Detection failed: ' + errMsg(e)
  }
}

async function domExpose() {
  const err = $('dom-err')
  err.classList.add('hidden')
  const domains = $('dom-domains').value.trim().split(/\s+/).filter(Boolean)
  const port = parseInt($('dom-port').value || '0', 10)
  const spec = {
    stackPath: $('dom-path').value.trim(),
    mainCompose: $('dom-compose').value.trim(),
    service: $('dom-service').value.trim(),
    domains,
    port,
    network: $('dom-network').value.trim(),
    overrideName: '',
  }
  if (!spec.stackPath || !spec.mainCompose || !spec.service || !domains.length || !port) {
    err.textContent = 'Fill in stack directory, compose file, service, at least one domain, and the port.'
    err.classList.remove('hidden')
    return
  }
  $('dom-form-view').classList.add('hidden')
  $('dom-run-view').classList.remove('hidden')
  const log = $('dom-log')
  log.textContent = ''
  $('dom-outcome').classList.add('hidden')
  $('dom-reset').classList.add('hidden')
  if (dom.exposeOff) { dom.exposeOff(); dom.exposeOff = null }
  dom.exposeOff = EventsOn('caddy-expose', (line) => {
    log.textContent += line + '\n'
    log.scrollTop = log.scrollHeight
  })
  try {
    await ExposeServiceDomain(state.selectedId, spec, $('dom-sudo').checked)
    domFinish('✓ Service exposed. Caddy obtains a certificate on first request to the domain.', 'ok')
  } catch (e) {
    domFinish('✗ ' + errMsg(e), 'err')
  } finally {
    if (dom.exposeOff) { dom.exposeOff(); dom.exposeOff = null }
    $('dom-reset').classList.remove('hidden')
  }
}

function domFinish(msg, kind) {
  const o = $('dom-outcome')
  o.className = kind === 'ok' ? 'ok-msg' : 'err-msg'
  o.textContent = msg
  o.classList.remove('hidden')
}

async function domCloudflare() {
  const msg = $('dom-cf-msg')
  msg.classList.add('hidden')
  const token = $('dom-cf-token').value.trim()
  const zone = $('dom-cf-zone').value.trim()
  const name = $('dom-cf-name').value.trim()
  const ip = $('dom-cf-ip').value.trim()
  if (!token || !zone || !name || !ip) {
    msg.className = 'err-msg'
    msg.textContent = 'Fill token, zone, record name and IPv4.'
    msg.classList.remove('hidden')
    return
  }
  const btn = $('dom-cf-apply')
  btn.disabled = true
  btn.textContent = 'Working…'
  try {
    await CloudflareUpsert(token, zone, name, ip, $('dom-cf-proxied').checked)
    msg.className = 'ok-msg'
    msg.textContent = '✓ DNS record set: ' + name + ' → ' + ip
  } catch (e) {
    msg.className = 'err-msg'
    msg.textContent = '✗ ' + errMsg(e)
  } finally {
    msg.classList.remove('hidden')
    btn.disabled = false
    btn.textContent = 'Create / update DNS'
  }
}

// ─────────── Tabs ───────────
// setActiveTab handles only the visual switch; switchTab also loads the tab's
// content. They're separate so the logs view can show the terminal panel
// without triggering an interactive shell.
function setActiveTab(name) {
  state.tab = name
  document.querySelectorAll('.tab').forEach((t) =>
    t.classList.toggle('active', t.dataset.tab === name)
  )
  document.querySelectorAll('.tab-panel').forEach((p) =>
    p.classList.toggle('active', p.id === 'tab-' + name)
  )
}

function switchTab(name) {
  setActiveTab(name)
  loadCurrentTab()
}

// ─────────── Modal ───────────
function openModal(vps = null) {
  const form = $('vps-form')
  form.reset()
  if (vps) {
    $('modal-title').textContent = 'Edit VPS'
    for (const [k, v] of Object.entries(vps)) {
      const f = form.elements.namedItem(k)
      if (f) f.value = v ?? ''
    }
  } else {
    $('modal-title').textContent = 'Add VPS'
    form.elements.namedItem('id').value = ''
    form.elements.namedItem('port').value = '22'
    form.elements.namedItem('authType').value = 'key'
  }
  toggleAuthFields(form.elements.namedItem('authType').value)
  $('vps-modal').classList.remove('hidden')
}

function closeModal() {
  $('vps-modal').classList.add('hidden')
}

function toggleAuthFields(type) {
  $('key-field').classList.toggle('hidden', type !== 'key')
  $('password-field').classList.toggle('hidden', type !== 'password')
}

// ─────────── Wire up ───────────
document.addEventListener('DOMContentLoaded', () => {
  // Load servers before projects: project rows label themselves from the server
  // list, so rendering them first would show "missing server" until reload.
  refreshVPSList().then(() => {
    refreshProjects()
    startFleetPolling()
  })
  refreshLocalAvailability()

  // Fleet topbar
  $('fleet-add-server').addEventListener('click', () => openModal())
  $('fleet-add-project').addEventListener('click', () => openProjectModal())
  $('fleet-proj-btn').addEventListener('click', (e) => {
    e.stopPropagation()
    toggleProjectMenu()
  })
  // Any click outside the dropdown closes it.
  document.addEventListener('click', (e) => {
    if (!e.target.closest('.fleet-proj-wrap')) toggleProjectMenu(false)
  })
  $('back-to-fleet').addEventListener('click', showFleetView)

  // Project modal
  $('project-cancel').addEventListener('click', () => $('project-modal').classList.add('hidden'))
  $('project-form').addEventListener('submit', submitProjectForm)
  $('project-server-mode').addEventListener('change', (e) => toggleProjectServerMode(e.target.value))
  $('np-authType').addEventListener('change', (e) => toggleNpAuth(e.target.value))

  // Modal
  $('modal-cancel').addEventListener('click', closeModal)
  $('vps-form').addEventListener('submit', async (e) => {
    e.preventDefault()
    const fd = new FormData(e.target)
    const data = Object.fromEntries(fd.entries())
    data.port = parseInt(data.port || '22', 10)
    try {
      if (data.id) {
        await UpdateVPS(data)
      } else {
        delete data.id
        await AddVPS(data)
      }
      closeModal()
      await refreshVPSList()
    } catch (err) {
      alert('Save failed: ' + errMsg(err))
    }
  })
  $('vps-form').elements.namedItem('authType').addEventListener('change', (e) => {
    toggleAuthFields(e.target.value)
  })

  // Topbar
  $('connect-btn').addEventListener('click', doConnect)
  $('disconnect-btn').addEventListener('click', doDisconnect)
  $('edit-vps-btn').addEventListener('click', () => {
    const v = state.vpses.find((x) => x.id === state.selectedId)
    if (v) openModal(v)
  })
  $('delete-vps-btn').addEventListener('click', async () => {
    if (!state.selectedId) return
    const v = state.vpses.find((x) => x.id === state.selectedId)
    if (!confirm(`Delete config for "${v?.name}"?`)) return
    await DeleteVPS(state.selectedId)
    delete fleet.conn[state.selectedId]
    delete fleet.stats[state.selectedId]
    delete fleet.containers[state.selectedId]
    if (fleet.expandedId === state.selectedId) fleet.expandedId = null
    state.selectedId = null
    state.connected = false
    await refreshVPSList()
    showFleetView()
  })

  // Tabs
  document.querySelectorAll('.tab').forEach((t) => {
    t.addEventListener('click', () => switchTab(t.dataset.tab))
  })

  // Ask
  $('ask-send').addEventListener('click', askSend)
  $('ask-stop').addEventListener('click', askStop)
  $('ask-input').addEventListener('input', askAutosize)
  $('ask-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); askSend() }
  })
  document.querySelectorAll('.ask-suggestion').forEach((b) => b.addEventListener('click', () => {
    $('ask-input').value = b.dataset.prompt
    askAutosize()
    askSend()
  }))
  $('ask-codex-link').addEventListener('click', (e) => {
    e.preventDefault()
    BrowserOpenURL('https://github.com/openai/codex')
  })

  // Maintenance
  $('maint-refresh').addEventListener('click', maintRefresh)
  $('maint-prune').addEventListener('click', () => maintPruneToggle(true))
  $('maint-prune-cancel').addEventListener('click', () => maintPruneToggle(false))
  $('maint-prune-run').addEventListener('click', maintRunPrune)

  // Provision
  $('prov-run').addEventListener('click', provisionRun)
  $('prov-reset').addEventListener('click', () => switchTab('provision'))

  // Domains
  $('dom-detect').addEventListener('click', domDetect)
  $('dom-expose').addEventListener('click', domExpose)
  $('dom-cf-apply').addEventListener('click', domCloudflare)
  $('dom-reset').addEventListener('click', () => switchTab('domains'))

  // Permissions modal
  $('perms-cancel').addEventListener('click', closePermsModal)
  $('perms-form').addEventListener('submit', submitPerms)

  // New folder modal
  $('mkdir-cancel').addEventListener('click', () => $('mkdir-modal').classList.add('hidden'))
  $('mkdir-form').addEventListener('submit', submitMkdir)

  // Editor
  $('editor-cancel').addEventListener('click', closeEditor)
  $('editor-save').addEventListener('click', saveEditor)
  $('editor-modal').addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeEditor()
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); saveEditor() }
  })

  // Files
  $('files-up').addEventListener('click', goUp)
  $('files-refresh').addEventListener('click', () => loadFiles(state.currentDir))
  $('files-go').addEventListener('click', () => loadFiles($('files-path').value))
  $('files-path').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') loadFiles(e.target.value)
  })
  $('files-mkdir').addEventListener('click', createDir)
  $('files-upload').addEventListener('click', uploadFile)

  // Docker
  $('docker-refresh').addEventListener('click', loadContainers)
  $('docker-all').addEventListener('change', (e) => { state.dockerShowAll = e.target.checked; loadContainers() })
  // Filtering is local to the already-fetched list, so no refetch on keystroke.
  $('docker-search').addEventListener('input', (e) => {
    state.dockerFilter = e.target.value
    renderContainers()
  })

  // Databases
  $('db-refresh').addEventListener('click', dbLoadContainers)
  $('db-sudo').addEventListener('change', dbLoadContainers)
  $('db-all').addEventListener('change', (e) => { state.dbShowAll = e.target.checked; dbLoadContainers() })
  $('db-container').addEventListener('change', (e) => {
    dbs.containerId = e.target.value
    dbs.database = null
    dbLoadDatabases()
  })
  $('db-database').addEventListener('change', (e) => {
    dbs.database = e.target.value
    dbs.table = null
    dbLoadTables()
  })
  $('db-mode-tables').addEventListener('click', () => { dbs.mode = 'tables'; dbShowMode() })
  $('db-mode-sql').addEventListener('click', () => { dbs.mode = 'sql'; dbShowMode() })
  $('db-prev').addEventListener('click', () => {
    if (dbs.page > 0) { dbs.page--; dbLoadRows() }
  })
  $('db-next').addEventListener('click', () => {
    if ((dbs.page + 1) * dbs.pageSize < dbs.total) { dbs.page++; dbLoadRows() }
  })
  $('db-insert').addEventListener('click', () => openRowModal('insert'))
  $('db-row-cancel').addEventListener('click', closeRowModal)
  $('db-row-form').addEventListener('submit', submitRowModal)
  $('db-sql-run').addEventListener('click', dbRunSQL)
  $('db-sql-input').addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') { e.preventDefault(); dbRunSQL() }
  })

  // Deploy
  $('deploy-add').addEventListener('click', () => openDeployModal())
  $('deploy-cancel').addEventListener('click', () => $('deploy-modal').classList.add('hidden'))
  $('deploy-form').addEventListener('submit', submitDeployForm)
  $('deploy-env-add').addEventListener('click', () => addEnvRow())
  $('deploy-env-import-toggle').addEventListener('click', () => {
    $('deploy-env-import').classList.toggle('hidden')
  })
  $('deploy-env-import-cancel').addEventListener('click', () => {
    $('deploy-env-import-text').value = ''
    $('deploy-env-import').classList.add('hidden')
  })
  $('deploy-env-import-choose').addEventListener('click', () => $('deploy-env-import-file').click())
  $('deploy-env-import-file').addEventListener('change', async (e) => {
    const file = e.target.files[0]
    if (!file) return
    $('deploy-env-import-text').value = await file.text()
    e.target.value = ''
  })
  $('deploy-env-import-apply').addEventListener('click', () => {
    const n = importEnvRows($('deploy-env-import-text').value)
    $('deploy-env-import-text').value = ''
    $('deploy-env-import').classList.add('hidden')
    if (n === 0) alert('No KEY=value lines found to import.')
  })
  $('deploy-run-back').addEventListener('click', () => {
    $('deploy-run-view').classList.add('hidden')
    $('deploy-list-view').classList.remove('hidden')
    deployRefresh()
  })

  // Migration
  $('mig-inspect').addEventListener('click', migInspect)
  $('mig-back').addEventListener('click', () => migShowStage('setup'))
  $('mig-run').addEventListener('click', migRun)
  $('mig-reset').addEventListener('click', () => migShowStage('setup'))
  $('mig-substack-back').addEventListener('click', () => migShowStage('setup'))
  $('mig-substack-run').addEventListener('click', migRunMulti)
  $('mig-src').addEventListener('change', () => {
    // Avoid src == dst — pick a different (non-local) target if needed.
    if ($('mig-src').value === $('mig-dst').value) {
      const other = state.vpses.find((v) => !v.isLocal && v.id !== $('mig-src').value)
      if (other) $('mig-dst').value = other.id
    }
  })
  $('mig-cleanup').addEventListener('click', async () => {
    const srcId = $('mig-src').value
    if (srcId && srcId !== state.selectedId) await selectVPS(srcId)
    $('cleanup-path').value = $('mig-src-path').value.trim()
    $('cleanup-sudo').checked = mig.useSudo
    switchTab('cleanup')
  })

  // Cleanup
  $('cleanup-inspect').addEventListener('click', cleanupInspect)
  $('cleanup-back').addEventListener('click', () => cleanupShowStage('setup'))
  $('cleanup-reset').addEventListener('click', () => cleanupShowStage('setup'))
  $('cleanup-run').addEventListener('click', cleanupRun)
  $('cleanup-confirm').addEventListener('input', () => {
    $('cleanup-run').disabled = $('cleanup-confirm').value.trim() !== $('cleanup-confirm-name').textContent
  })

  // Backups
  $('backups-refresh').addEventListener('click', backupsRefresh)
  $('backups-sudo').addEventListener('change', backupsRefresh)
  $('backups-add').addEventListener('click', () => bakOpenForm(null))
  $('bak-cancel').addEventListener('click', () => bakShowView('list-view'))
  $('bak-detect').addEventListener('click', bakDetect)
  $('bak-provider').addEventListener('change', (e) => bakApplyProvider(e.target.value))
  $('bak-save').addEventListener('click', bakSave)
  $('backups-run-back').addEventListener('click', () => { bakShowView('list-view'); backupsRefresh() })
  $('backups-snap-back').addEventListener('click', () => bakShowView('list-view'))
  $('bak-restore-cancel').addEventListener('click', () => bakShowView('snap-view'))
  $('bak-restore-go').addEventListener('click', bakDoRestore)
  $('bak-restore-confirm').addEventListener('change', (e) => { $('bak-restore-go').disabled = !e.target.checked })
  $('bak-restore-vps').addEventListener('change', bakRestoreToggleCreds)
  $('bak-restore-runback').addEventListener('click', () => { bakShowView('list-view'); backupsRefresh() })
  document.querySelectorAll('.bak-preset').forEach((b) =>
    b.addEventListener('click', (e) => { e.preventDefault(); $('bak-schedule').value = b.dataset.cron }))

  // Terminal: keep the PTY size in sync with the panel when the window resizes.
  let resizeTimer = null
  window.addEventListener('resize', () => {
    if (state.tab !== 'terminal') return
    clearTimeout(resizeTimer)
    resizeTimer = setTimeout(fitTerm, 120)
  })
})
