// Frontend entrypoint. Wails generates bindings into ../wailsjs/* on `wails dev`
// / `wails build`, so if you see "Cannot find module" errors, run `wails dev` once
// (or `wails generate module`) to create them.
import './style.css'

import {
  ListVPS, AddVPS, UpdateVPS, DeleteVPS,
  Connect, Disconnect, IsConnected,
  ListFiles, DownloadFile, UploadFile, DeleteRemoteFile, DefaultDownloadDir,
  ListContainers, RestartContainer, StopContainer, StartContainer, ContainerLogs,
  RunCommand, ChooseSavePath, ChooseOpenPath,
} from '../wailsjs/go/main/App'

// ─────────── State ───────────
const state = {
  vpses: [],
  selectedId: null,
  currentDir: '/',
  connected: false,
  tab: 'files',
  history: [],
  historyIdx: 0,
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
  try { await Disconnect(state.selectedId) } catch {}
  state.connected = false
  updateStatusUI()
  renderVPSList()
}

function loadCurrentTab() {
  if (!state.connected) return
  if (state.tab === 'files') {
    if (!state.currentDir) state.currentDir = '/'
    $('files-path').value = state.currentDir
    loadFiles(state.currentDir)
  } else if (state.tab === 'docker') {
    loadContainers()
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
      const dl = document.createElement('button')
      dl.textContent = 'Download'
      dl.onclick = (e) => { e.stopPropagation(); downloadFile(f.path, f.name) }
      actions.appendChild(dl)
    }
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
    switchTab('terminal')
    appendOutput(`--- logs ${name} (last 200 lines) ---`, 'info')
    appendOutput(logs)
  } catch (e) {
    alert('Logs failed: ' + errMsg(e))
  }
}

// ─────────── Terminal ───────────
const PTY_CMD_RE = /^(nano|vi|vim|nvim|emacs|less|more|top|htop|man|watch|lynx|w3m|mysql|psql|sqlite3|python[23]?|ipython|node|irb|pry|ssh|telnet|ftp|sftp)\b/

async function runCmd(cmd) {
  appendOutput(`$ ${cmd}`, 'cmd')
  if (PTY_CMD_RE.test(cmd.trim())) {
    appendOutput(
      'error: interactive commands require a TTY and are not supported in this terminal.\n' +
      'Tip: use "cat <file>" to view files, or use the Files tab to download and edit them.',
      'err'
    )
    return
  }
  try {
    const res = await RunCommand(state.selectedId, cmd)
    if (res.stdout) appendOutput(res.stdout)
    if (res.stderr) appendOutput(res.stderr, 'err')
    if (res.exitCode !== 0) appendOutput(`[exit ${res.exitCode}]`, 'info')
  } catch (e) {
    appendOutput('error: ' + errMsg(e), 'err')
  }
}

function appendOutput(text, cls = '') {
  const out = $('terminal-output')
  const div = document.createElement('div')
  if (cls) div.className = cls
  div.textContent = text
  out.appendChild(div)
  out.scrollTop = out.scrollHeight
}

// ─────────── Tabs ───────────
function switchTab(name) {
  state.tab = name
  document.querySelectorAll('.tab').forEach((t) =>
    t.classList.toggle('active', t.dataset.tab === name)
  )
  document.querySelectorAll('.tab-panel').forEach((p) =>
    p.classList.toggle('active', p.id === 'tab-' + name)
  )
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

  // Files
  $('files-up').addEventListener('click', goUp)
  $('files-refresh').addEventListener('click', () => loadFiles(state.currentDir))
  $('files-go').addEventListener('click', () => loadFiles($('files-path').value))
  $('files-path').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') loadFiles(e.target.value)
  })
  $('files-upload').addEventListener('click', uploadFile)

  // Docker
  $('docker-refresh').addEventListener('click', loadContainers)

  // Terminal
  $('cmd-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      const cmd = e.target.value.trim()
      if (!cmd) return
      state.history.push(cmd)
      state.historyIdx = state.history.length
      e.target.value = ''
      runCmd(cmd)
    } else if (e.key === 'ArrowUp') {
      if (state.history.length === 0) return
      state.historyIdx = Math.max(0, state.historyIdx - 1)
      e.target.value = state.history[state.historyIdx] || ''
      e.preventDefault()
    } else if (e.key === 'ArrowDown') {
      state.historyIdx = Math.min(state.history.length, state.historyIdx + 1)
      e.target.value = state.history[state.historyIdx] || ''
      e.preventDefault()
    }
  })
})
