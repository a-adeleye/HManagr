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
  SetSudoPassword, HasSudoPassword,
  FindComposeFile, InspectMigration, RunMigration,
  ReadRemoteFile, WriteRemoteFile,
  ListContainers, RestartContainer, StopContainer, StartContainer, ContainerLogs,
  StartShell, WriteShell, ResizeShell, CloseShell,
  ClipboardText, SetClipboardText, ChooseSavePath, ChooseOpenPath,
  ListDBContainers, DBListDatabases, DBListTables, DBTableColumns, DBTableRows,
  DBQuery, DBInsertRow, DBUpdateRow, DBDeleteRow,
  ListDeployments, SaveDeployment, DeleteDeployment, RunDeploy,
  LocalAvailable, LocalUnavailableReason, LocalStartDir,
  ListProjects, SaveProject, DeleteProject,
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

// Whether the currently-selected environment is the virtual local machine.
const isLocalSel = () => state.selectedId === 'local'

// Local-mode availability (a host POSIX shell exists), resolved once at startup.
state.localAvailable = true
state.localReason = ''

// Projects: named server+path bookmarks. sidebar toggles the list shown.
state.sidebar = 'servers'      // 'servers' | 'projects'
state.projects = []
state.activeProjectId = null   // project currently open (for highlight)
state.projectPath = null       // deploy path the current session is rooted at
state.pendingProjectPath = null // set by openProject, consumed by selectVPS
// When in a project, Docker/DB lists scope to that project's compose stack
// unless the user opts to see everything on the server.
state.dockerShowAll = false
state.dbShowAll = false

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
  renderVPSList()
}

function renderVPSList() {
  const ul = $('vps-list')
  ul.innerHTML = ''
  for (const v of state.vpses) {
    const li = document.createElement('li')
    const localUnavail = v.isLocal && !state.localAvailable
    li.className = 'vps-item' + (state.selectedId === v.id ? ' active' : '') +
      (v.isLocal ? ' local' : '') + (localUnavail ? ' unavailable' : '')
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
    li.querySelector('.host').textContent = v.isLocal
      ? (localUnavail ? (state.localReason || 'unavailable on this machine') : '🖥  this machine')
      : `${v.user}@${v.host}:${v.port || 22}`
    if (localUnavail) li.title = state.localReason || ''
    li.addEventListener('click', () => selectVPS(v.id))
    ul.appendChild(li)
  }
  // The synthetic Local entry is always present, so the list is never empty;
  // show the "add a server" hint only when there are no real (SSH) servers.
  if (!state.vpses.some((v) => !v.isLocal)) {
    const li = document.createElement('li')
    li.className = 'vps-empty muted'
    li.textContent = 'No servers yet. Click + to add one.'
    ul.appendChild(li)
  }
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
  $('empty-state').classList.add('hidden')
  $('vps-view').classList.remove('hidden')
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
      state.currentDir = '/'
    }
  }
  // Each (re)selection starts scoped: a project shows its own stack, a plain
  // server shows everything (projectPath null makes the scope a no-op).
  state.dockerShowAll = false
  state.dbShowAll = false
  updateStatusUI()
  renderVPSList()
  renderProjectList()
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
  renderProjectList()
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

function switchSidebar(side) {
  state.sidebar = side
  document.querySelectorAll('.side-btn').forEach((b) => b.classList.toggle('active', b.dataset.side === side))
  $('vps-list').classList.toggle('hidden', side !== 'servers')
  $('project-list').classList.toggle('hidden', side !== 'projects')
  $('add-btn').title = side === 'projects' ? 'Add project' : 'Add server'
}

function renderProjectList() {
  const ul = $('project-list')
  ul.innerHTML = ''
  if (!state.projects.length) {
    ul.innerHTML = '<li class="vps-empty muted">No projects yet. Click + to add one.</li>'
    return
  }
  for (const p of state.projects) {
    const li = document.createElement('li')
    li.className = 'vps-item' + (state.activeProjectId === p.id ? ' active' : '')
    const isConnected = state.activeProjectId === p.id && state.connected
    li.innerHTML = `
      <div class="name-row">
        <span class="dot ${isConnected ? 'connected' : ''}"></span>
        <span class="name"></span>
        <span class="proj-actions"></span>
      </div>
      <div class="host"></div>
      <div class="proj-path"></div>
    `
    li.querySelector('.name').textContent = p.name
    li.querySelector('.host').textContent = serverLabel(p.vpsId)
    li.querySelector('.proj-path').textContent = p.path
    const actions = li.querySelector('.proj-actions')
    const ed = document.createElement('button')
    ed.textContent = 'Edit'
    ed.onclick = (e) => { e.stopPropagation(); openProjectModal(p) }
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
    li.addEventListener('click', () => openProject(p.id))
    ul.appendChild(li)
  }
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
    card.querySelector('.repo').textContent = `${d.repoUrl} @ ${d.branch || 'main'}`
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

function openDeployModal(d = null) {
  const form = $('deploy-form')
  form.reset()
  $('deploy-env-rows').innerHTML = ''
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
  log.textContent = `⚡ ${d.repoUrl} @ ${d.branch || 'main'} → ${d.path}\n` +
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
    sections.push(`<h4>Built images (${inv.buildImages.length})</h4>`)
    sections.push('<ul>' + inv.buildImages.map((n) =>
      `<li><code>${n}</code> <span class="muted">— shipped prebuilt (save/load), no rebuild on target</span></li>`
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
    composeFile: mig.composeFile,
    volumes: (mig.inventory.volumes || []).map((v) => v.name),
    envFiles: mig.inventory.envFiles || [],
    externalNetworks: mig.inventory.externalNetworks || [],
    buildImages: mig.inventory.buildImages || [],
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
  // Lines prefixed with a zero-width space are in-place progress updates: they
  // replace the previous progress line instead of appending, so a transfer
  // counter ticks in place (terminal-style) rather than flooding the log.
  const PROG_MARK = '​'
  mig.logOff = EventsOn('migration:log', (line) => {
    const el = $('mig-log')
    let txt = el.textContent
    if (txt.endsWith('\n')) {
      const body = txt.slice(0, -1)
      const nl = body.lastIndexOf('\n')
      if (body.slice(nl + 1).startsWith(PROG_MARK)) {
        txt = txt.slice(0, nl + 1) // drop the prior progress line
      }
    }
    el.textContent = txt + line + '\n'
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
  // Load servers before projects: project rows label themselves from the server
  // list, so rendering them first would show "missing server" until reload.
  refreshVPSList().then(() => refreshProjects())
  refreshLocalAvailability()

  // Sidebar: the + button adds whatever the active list shows.
  $('add-btn').addEventListener('click', () => {
    if (state.sidebar === 'projects') openProjectModal()
    else openModal()
  })
  document.querySelectorAll('.side-btn').forEach((b) =>
    b.addEventListener('click', () => switchSidebar(b.dataset.side)))

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
  $('docker-all').addEventListener('change', (e) => { state.dockerShowAll = e.target.checked; loadContainers() })

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
  $('mig-src').addEventListener('change', () => {
    // Avoid src == dst — pick a different (non-local) target if needed.
    if ($('mig-src').value === $('mig-dst').value) {
      const other = state.vpses.find((v) => !v.isLocal && v.id !== $('mig-src').value)
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
