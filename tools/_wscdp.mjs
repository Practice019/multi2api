
import net from 'node:net'
import crypto from 'node:crypto'
import http from 'node:http'

function httpGet(url) {
  return new Promise((res, rej) => {
    http.get(url, (r) => { let d=''; r.on('data', c => d+=c); r.on('end', () => res({status:r.statusCode, body:d})) }).on('error', rej)
  })
}

// 极简 WebSocket 客户端：握手 + 发送文本帧 + 读帧
async function cdpCall(wsUrl, method, params) {
  const u = new URL(wsUrl)
  return new Promise((resolve, reject) => {
    const key = crypto.randomBytes(16).toString('base64')
    const sock = net.connect(Number(u.port), u.hostname, () => {
      sock.write(
        'GET ' + u.pathname + ' HTTP/1.1\r\n' +
        'Host: ' + u.host + '\r\n' +
        'Upgrade: websocket\r\n' +
        'Connection: Upgrade\r\n' +
        'Sec-WebSocket-Key: ' + key + '\r\n' +
        'Sec-WebSocket-Version: 13\r\n\r\n'
      )
    })
    let handshook = false
    let buf = Buffer.alloc(0)
    const timer = setTimeout(() => { sock.destroy(); reject(new Error('超时')) }, 15000)

    sock.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk])
      if (!handshook) {
        const idx = buf.indexOf('\r\n\r\n')
        if (idx < 0) return
        const head = buf.slice(0, idx).toString()
        if (!head.includes('101')) { clearTimeout(timer); sock.destroy(); return reject(new Error('握手失败: ' + head.split('\r\n')[0])) }
        handshook = true
        buf = buf.slice(idx + 4)
        // 发请求帧（客户端必须 mask）
        const payload = Buffer.from(JSON.stringify({ id: 1, method, params }))
        const mask = crypto.randomBytes(4)
        const masked = Buffer.alloc(payload.length)
        for (let i = 0; i < payload.length; i++) masked[i] = payload[i] ^ mask[i % 4]
        let header
        if (payload.length < 126) header = Buffer.from([0x81, 0x80 | payload.length])
        else if (payload.length < 65536) { header = Buffer.alloc(4); header[0]=0x81; header[1]=0x80|126; header.writeUInt16BE(payload.length, 2) }
        else { header = Buffer.alloc(10); header[0]=0x81; header[1]=0x80|127; header.writeBigUInt64BE(BigInt(payload.length), 2) }
        sock.write(Buffer.concat([header, mask, masked]))
      }
      // 解析服务端帧（不 mask）
      while (buf.length >= 2) {
        const b1 = buf[1]
        const masked = (b1 & 0x80) !== 0
        let len = b1 & 0x7f
        let off = 2
        if (len === 126) { if (buf.length < 4) return; len = buf.readUInt16BE(2); off = 4 }
        else if (len === 127) { if (buf.length < 10) return; len = Number(buf.readBigUInt64BE(2)); off = 10 }
        if (masked) off += 4
        if (buf.length < off + len) return
        const payload = buf.slice(off, off + len)
        buf = buf.slice(off + len)
        try {
          const msg = JSON.parse(payload.toString())
          if (msg.id === 1) { clearTimeout(timer); sock.destroy(); resolve(msg) }
        } catch {}
      }
    })
    sock.on('error', (e) => { clearTimeout(timer); reject(e) })
  })
}

const PORT = 9334
const targets = JSON.parse((await httpGet('http://127.0.0.1:' + PORT + '/json/list')).body)
const page = targets.find(t => t.type === 'page' && t.url.includes('monkeycode')) || targets.find(t => t.type === 'page')
if (!page) { console.log('无可用页面'); process.exit(1) }
console.log('  目标: ' + page.url.slice(0,60))
const r = await cdpCall(page.webSocketDebuggerUrl, 'Storage.getCookies', {})
if (r.error) { console.log('  错误: ' + JSON.stringify(r.error)); process.exit(1) }
const all = (r.result?.cookies || [])
console.log('  cookie 总数: ' + all.length)
const mk = all.filter(c => String(c.domain).includes('monkeycode'))
console.log('  monkeycode 相关: ' + mk.length)
mk.forEach(c => console.log('    ' + c.name + ' @' + c.domain + '  len=' + String(c.value).length))
