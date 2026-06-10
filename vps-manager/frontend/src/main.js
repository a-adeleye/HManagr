// Frontend entrypoint. Wails generates bindings into ../wailsjs/* on `wails dev`
// / `wails build`, so if you see "Cannot find module" errors, run `wails dev` once
// (or `wails generate module`) to create them.
import './style.css'
import '@xterm/xterm/css/xterm.css'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'

import {
  ListVPS, AddVPS, UpdateVPS, DeleteVPS,
  Connect, Disconnect, IsConnected,
  ListFiles, DownloadFile, UploadFile, DeleteRemoteFile, MakeDir, DefaultDownloadDir,
  StatRemoteFile, ChmodRemoteFile, ChownRemoteFile,
  SetSudoPassword, HasSudoPassword, ProbeSudo,
  FindComposeFile, InspectMigration, RunMigration,
  ReadRemoteFile, WriteRemoteFile,
  ListContainers, RestartContainer, StopContainer, StartContainer, ContainerLogs,
  StartShell, WriteShell, ResizeShell, CloseShell,
  ClipboardText, SetClipboardText, ChooseSavePath, ChooseOpenPath,
} from '../wailsjs/go/main/App'
import { EventsOn } from '../wailsjs/runtime/runtime'

// ─────────── State ───────────
const state = {
  vpses: [],
  selectedId: null,
  currentDir: '/',
  connected: false,
  tab: 'files',
  editorPath: null,
}

// Interactive terminal (xterm.js) bound to a single PTY shell at a time.
const term = {
  inst: null,        // Terminal instance (created lazily)
  fit: null,         // FitAddon
  vpsId: null,       // VPS whose shell is currently attached
  offOutput: null,   // EventsOn cancel fn for shell:output
  offExit: null,     // EventsOn cancel fn for shell:exit
}

// ─────────── Helpers ───────────
const $ = (id) => document.getElementById(id)
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
  renderVPSList()
}

function renderVPSList() {
  const ul = $('vps-list')
  ul.innerHTML = ''
  if (state.vpses.length === 0) {
    ul.innerHTML = '<li class="vps-empty muted">No servers yet. Click + to add one.</li>'
    return
  }
  for (const v of state.vpses) {
    const li = document.createElement('li')
    li.className = 'vps-item' + (state.selectedId === v.id ? ' active' : '')
    li.dataset.id = v.id
    const isConnected = state.selectedId === v.id && state.connected
    li.innerHTML = `
      <div class="name-row">
        <span class="dot ${isConnected ? 'connected' : ''}"></span>
        <span class="name"></span>
      </div>
      <div class="host"></div>
    `
    li.querySelector('.name').textContent = v.name
    li.querySelector('.host').textContent = `${v.user}@${v.host}:${v.port || 22}`
    li.addEventListener('click', () => selectVPS(v.id))
    ul.appendChild(li)
  }
}

async function selectVPS(id) {
  state.selectedId = id
  const v = state.vpses.find((x) => x.id === id)
  if (!v) return
  try {
    state.connected = await IsConnected(id)
  } catch {
    state.connected = false
  }
  $('empty-state').classList.add('hidden')
  $('vps-view').classList.remove('hidden')
  $('vps-name').textContent = v.name
  $('vps-host').textContent = `${v.user}@${v.host}:${v.port || 22}`
  updateStatusUI()
  renderVPSList()
  if (state.connected) {
    loadCurrentTab()
  } else {
    $('files-list').innerHTML = '<p class="muted center-msg">Connect to browse files.</p>'
    $('docker-list').innerHTML = '<p class="muted center-msg">Connect to view containers.</p>'
    resetTerminalPanel()
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

// ─────────── Connection ───────────
async function doConnect() {
  if (!state.selectedId) return
  const btn = $('connect-btn')
  btn.textContent = 'Connecting…'
  btn.disabled = true
  $('status-pill').textContent = 'Connecting'
  $('status-pill').className = 'status-pill connecting'
  try {
    await Connect(state.selectedId)
    state.connected = true
    updateStatusUI()
    renderVPSList()
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
  updateStatusUI()
  renderVPSList()
}

// Restore the terminal panel to its disconnected state.
function resetTerminalPanel() {
  if (term.inst) term.inst.reset()
  $('terminal').classList.add('hidden')
  $('terminal-placeholder').classList.remove('hidden')
}

function loadCurrentTab() {
  // The migration tab uses two VPSes chosen from dropdowns, not the currently-
  // selected one, so it stays accessible regardless of connection state.
  if (state.tab === 'migration') {
    migOpen()
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
    openShell()
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

// ensureSudoReady probes the VPS to see whether sudo needs a password right now
// (it may not — NOPASSWD setups exist, and a previous session may have cached
// one that still works). Only prompts the user if a password is actually
// required. Returns true if sudo is usable, false if the user cancels or the
// account isn't allowed to sudo at all.
async function ensureSudoReady(id) {
  let probe
  try { probe = await ProbeSudo(id) } catch (e) {
    alert('Sudo probe failed: ' + errMsg(e))
    return false
  }
  if (probe === 'ok') return true
  if (probe === 'denied') {
    alert('This user is not allowed to use sudo on the selected VPS.')
    return false
  }
  return await ensureSudoPassword(id, true)
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
  const parts = dir.replace(/\/$/, '').split('/')
  parts.pop()
  loadFiles(parts.join('/') || '/')
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
  list.innerHTML = '<p class="muted center-msg">Loading…</p>'
  try {
    const containers = await ListContainers(state.selectedId) || []
    renderContainers(containers)
  } catch (e) {
    list.innerHTML = `<p class="muted center-msg">Error: ${errMsg(e)}</p>`
  }
}

function renderContainers(containers) {
  const list = $('docker-list')
  list.innerHTML = ''
  if (!containers.length) {
    list.innerHTML = '<p class="muted center-msg">No containers found.</p>'
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

  // Already attached to this VPS's shell — just refit and focus.
  if (term.vpsId === state.selectedId) {
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
  for (const id of ['mig-setup', 'mig-inventory', 'mig-run-view']) {
    $(id).classList.toggle('hidden', id !== 'mig-' + name && id !== name)
  }
}

function migPopulateVpsSelects() {
  const opts = state.vpses.map((v) => `<option value="${v.id}">${v.name} (${v.user}@${v.host})</option>`).join('')
  for (const id of ['mig-src', 'mig-dst']) {
    const sel = $(id)
    const prev = sel.value
    sel.innerHTML = opts
    if (prev) sel.value = prev
  }
  // Sensible defaults: source = currently-selected VPS, target = first other.
  if (state.selectedId) $('mig-src').value = state.selectedId
  const others = state.vpses.filter((v) => v.id !== $('mig-src').value)
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

  // Inspect only touches source — probe sudo there. Target is probed at Run
  // time so the user doesn't get back-to-back prompts up front.
  if (useSudo && !(await ensureSudoReady(srcId))) return

  $('mig-setup-err').classList.add('hidden')
  const btn = $('mig-inspect')
  btn.disabled = true
  btn.textContent = 'Inspecting…'
  try {
    const composeFile = await FindComposeFile(srcId, srcPath)
    const inv = await InspectMigration(srcId, srcPath, useSudo)
    mig.inventory = inv
    mig.composeFile = composeFile
    mig.useSudo = useSudo
    migRenderInventory(inv, srcPath, dstPath, composeFile)
    migShowStage('inventory')
  } catch (e) {
    err('Inspect failed: ' + errMsg(e))
  } finally {
    btn.disabled = false
    btn.textContent = 'Inspect stack'
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
      `<li><strong>${s.name}</strong> — <code>${s.image}</code>${s.isPostgres ? ' <span class="muted">[postgres]</span>' : ''}</li>`
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
    composeFile: mig.composeFile,
    volumes: (mig.inventory.volumes || []).map((v) => v.name),
    envFiles: mig.inventory.envFiles || [],
  }

  // When sudo's on, both VPSes need to be ready before the run starts — the
  // migration goroutine can't pop modals from the backend. Probe first so we
  // only prompt for a password when one's actually needed.
  if (mig.useSudo) {
    if (!(await ensureSudoReady(srcId))) return
    if (!(await ensureSudoReady(dstId))) return
  }

  mig.running = true
  $('mig-log').textContent = ''
  $('mig-outcome').classList.add('hidden')
  $('mig-reset').classList.add('hidden')
  migShowStage('run-view')
  // First log line tells the user, in writing, whether sudo is actually in
  // effect for this run — so a forgotten checkbox can't quietly cause the
  // "mkdir: Permission denied" trap.
  const el = $('mig-log')
  el.textContent = mig.useSudo
    ? '⚡ Sudo: ON — remote commands prefixed with sudo on both VPSes.\n\n'
    : '⚡ Sudo: off — running as the SSH user on both VPSes.\n\n'

  // Subscribe before kicking off so early lines aren't missed.
  mig.logOff = EventsOn('migration:log', (line) => {
    const el = $('mig-log')
    el.textContent += line + '\n'
    el.scrollTop = el.scrollHeight
  })
  mig.doneOff = EventsOn('migration:done', (errStr) => {
    mig.running = false
    const out = $('mig-outcome')
    if (errStr) {
      out.textContent = '✗ Migration failed: ' + errStr
      out.className = 'err'
    } else {
      out.textContent = '✓ Migration completed. Verify the target before shutting down source.'
      out.className = 'ok'
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
  refreshVPSList()

  // Sidebar
  $('add-vps-btn').addEventListener('click', () => openModal())

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
    state.selectedId = null
    state.connected = false
    $('vps-view').classList.add('hidden')
    $('empty-state').classList.remove('hidden')
    refreshVPSList()
  })

  // Tabs
  document.querySelectorAll('.tab').forEach((t) => {
    t.addEventListener('click', () => switchTab(t.dataset.tab))
  })

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

  // Migration
  $('mig-inspect').addEventListener('click', migInspect)
  $('mig-back').addEventListener('click', () => migShowStage('setup'))
  $('mig-run').addEventListener('click', migRun)
  $('mig-reset').addEventListener('click', () => migShowStage('setup'))
  $('mig-src').addEventListener('change', () => {
    // Avoid src == dst — pick a different target if needed.
    if ($('mig-src').value === $('mig-dst').value) {
      const other = state.vpses.find((v) => v.id !== $('mig-src').value)
      if (other) $('mig-dst').value = other.id
    }
  })

  // Terminal: keep the PTY size in sync with the panel when the window resizes.
  let resizeTimer = null
  window.addEventListener('resize', () => {
    if (state.tab !== 'terminal') return
    clearTimeout(resizeTimer)
    resizeTimer = setTimeout(fitTerm, 120)
  })
})
