import { createLibp2p } from 'libp2p'
import { webRTC } from '@libp2p/webrtc'
import { webSockets } from '@libp2p/websockets'
import { circuitRelayTransport } from '@libp2p/circuit-relay-v2'
import { identify } from '@libp2p/identify'
import { noise } from '@chainsafe/libp2p-noise'
import { yamux } from '@chainsafe/libp2p-yamux'
import { multiaddr } from '@multiformats/multiaddr'
import { fromString as uint8FromString } from 'uint8arrays/from-string'
import { toString as uint8ToString } from 'uint8arrays/to-string'

const args = process.argv.slice(2)
const mode = args[0]
const relayAddr = args[1]
const protocol = args[2]

const done = (ok, message) => {
  if (ok) {
    console.log(JSON.stringify({ type: 'done', ok: true }))
  } else {
    console.log(JSON.stringify({ type: 'done', ok: false, error: message }))
  }
  process.exitCode = ok ? 0 : 1
}

process.on('unhandledRejection', (err) => {
  console.error(err)
  done(false, err?.message ?? String(err))
})

if (!mode || !relayAddr || !protocol) {
  console.error('Usage: interop.js <listen|dial> <relayAddr> <protocol> [targetAddr] [payload]')
  process.exit(2)
}

async function createNode() {
  return await createLibp2p({
    addresses: {
      listen: ['/p2p-circuit', '/webrtc']
    },
    transports: [webSockets(), webRTC(), circuitRelayTransport()],
    connectionEncrypters: [noise()],
    streamMuxers: [yamux()],
    connectionManager: {
      dialTimeout: 30000
    },
    services: {
      identify: identify()
    }
  })
}

async function waitForWebRTCAddr(node, timeoutMs) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    const addr = node.getMultiaddrs().find((entry) => entry.toString().includes('/webrtc'))
    if (addr) {
      return addr.toString()
    }
    await new Promise((resolve) => setTimeout(resolve, 200))
  }
  return ''
}

async function readAll(source) {
  const chunks = []
  for await (const chunk of source) {
    chunks.push(chunk)
  }
  const total = chunks.reduce((sum, chunk) => sum + chunk.length, 0)
  const merged = new Uint8Array(total)
  let offset = 0
  for (const chunk of chunks) {
    merged.set(chunk, offset)
    offset += chunk.length
  }
  return merged
}

async function main() {
  const node = await createNode()
  await node.start()
  await node.dial(multiaddr(relayAddr))

  const webrtcAddr = await waitForWebRTCAddr(node, 20000)
  if (!webrtcAddr) {
    await node.stop()
    done(false, 'no webrtc addr')
    return
  }
  console.log(JSON.stringify({ type: 'ready', webrtcAddr }))

  if (mode === 'listen') {
    const expected = args[3] ?? ''
    node.handle(protocol, async ({ stream }) => {
      try {
        const data = await readAll(stream.source)
        const received = uint8ToString(data)
        if (received !== expected) {
          done(false, `payload mismatch: ${received}`)
          return
        }
        done(true)
      } catch (err) {
        done(false, err?.message ?? String(err))
      } finally {
        await node.stop()
      }
    })
    return
  }

  if (mode === 'dial') {
    const targetAddr = args[3]
    const payload = args[4] ?? ''
    if (!targetAddr) {
      await node.stop()
      done(false, 'missing target addr')
      return
    }
    try {
      const onProgress = (evt) => {
        console.error(`progress ${evt.type}`)
      }
      const { stream } = await node.dialProtocol(multiaddr(targetAddr), protocol, {
        onProgress
      })
      const payloadBytes = uint8FromString(payload)
      await stream.sink(
        (async function* () {
          yield payloadBytes
        })()
      )
      await node.stop()
      done(true)
    } catch (err) {
      await node.stop()
      done(false, err?.message ?? String(err))
    }
    return
  }

  await node.stop()
  done(false, `unknown mode: ${mode}`)
}

main().catch((err) => done(false, err?.message ?? String(err)))
