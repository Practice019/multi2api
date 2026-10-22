import { execFile, spawn } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'

const EDGE = 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'
const PROF = 'D:\\project_GIT\\workbuddy2api\\data\\_edgeprof'
const PORT = 9333
try { fs.rmSync(PROF, { recursive: true, force: true }) } catch {}
fs.mkdirSync(PROF, { recursive: true })

function httpGet(url) {
  return new Promise((res, rej) => {
    http.get(url, (r) => { let d=''; r.on('data', c => d+=c); r.on('end', () => res({status:r.statusCode, body:d})) }).on('error', rej)
  })
}
function httpPut(url) {
  return new Promise((res, rej) => {
    const u = new URL(url)
    const r = http.request({ hostname: u.hostname, port: u.port, path: u.pathname+u.search, method: 'PUT' }, (x) => { let d=''; x.on('data', c=>d+=c); x.on('end', ()=>res({status:x.statusCode, body:d})) })
    r.on('error', rej); r.end()
  })
}

const child = spawn(EDGE, [
  '--remote-debugging-port=' + PORT,
  '--user-data-dir=' + PROF,
  '--no-first-run', '--no-default-browser-check',
  'about:blank',
], { detached: true, stdio: 'ignore' })
child.unref()
console.log('  Edge pid=' + child.pid)

// 等 CDP 起来
let version = null
for (let i = 0; i < 30; i++) {
  await new Promise(r => setTimeout(r, 500))
  try { const v = await httpGet('http://127.0.0.1:' + PORT + '/json/version'); if (v.status === 200) { version = JSON.parse(v.body); break } } catch {}
}
console.log('  CDP: ' + (version ? version.Browser : '未就绪'))

if (version) {
  const targets = JSON.parse((await httpGet('http://127.0.0.1:' + PORT + '/json/list')).body)
  const page = targets.find(t => t.type === 'page')
  console.log('  page target: ' + (page ? page.url : '无'))
  // PUT /json/new?url= 建新页
  const nu = await httpPut('http://127.0.0.1:' + PORT + '/json/new?url=https://monkeycode-ai.net/')
  console.log('  新建页: HTTP ' + nu.status)
  await new Promise(r => setTimeout(r, 3000))
  const t2 = JSON.parse((await httpGet('http://127.0.0.1:' + PORT + '/json/list')).body)
  console.log('  页面数: ' + t2.length)
  t2.forEach(t => console.log('    ' + t.type + '  ' + t.url.slice(0,80)))
  // 用 HTTP 方式取 cookie：CDP 只有 WebSocket，但 /json/protocol 可看能力
  console.log('  提示: cookie 需要 WebSocket (Storage.getCookies)')
}
try { process.kill(child.pid) } catch {}
try { execFile('taskkill', ['/PID', String(child.pid), '/T', '/F']) } catch {}
