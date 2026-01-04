//go:build interop
// +build interop

package libp2pwebrtc_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	circuitv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/require"
)

const interopProtocol = "/libp2p/webrtc-interop/1.0.0"

type jsEvent struct {
	Type       string `json:"type"`
	WebRTCAddr string `json:"webrtcAddr"`
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	Raw        string `json:"-"`
}

type jsProc struct {
	cmd      *exec.Cmd
	events   chan jsEvent
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	doneOnce chan struct{}
}

func TestWebRTCInterop_JSGo(t *testing.T) {
	if testing.Short() {
		t.Skip("interop tests disabled in -short mode")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}

	jsDir := filepath.Join("interop", "js")
	if _, err := os.Stat(filepath.Join(jsDir, "node_modules")); err != nil {
		t.Skipf("missing %s/node_modules; run npm install in %s", jsDir, jsDir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	relay, relayInfo, relayAddr := startRelayHost(t)
	defer relay.Close()

	goNode := startWebRTCNode(t)
	defer goNode.Close()

	require.NoError(t, goNode.Connect(ctx, relayInfo))
	_, err := circuitv2.Reserve(ctx, goNode, relayInfo)
	require.NoError(t, err)

	waitForWebRTCAddr(t, goNode, 20*time.Second)
	goDialAddr := buildRelayWebRTCAddr(relayAddr, goNode.ID())

	t.Run("js-dial-go-receive", func(t *testing.T) {
		payload := "hello-from-js"
		recvCh := make(chan string, 1)
		goNode.SetStreamHandler(protocol.ID(interopProtocol), func(s network.Stream) {
			defer s.Close()
			data, err := io.ReadAll(s)
			if err != nil {
				return
			}
			recvCh <- string(data)
		})

		js := startJS(t, jsDir, "dial", relayAddr.String(), interopProtocol, goDialAddr.String(), payload)
		defer js.Kill()

		recv := waitStringWithJS(t, recvCh, js, 20*time.Second)
		require.Equal(t, payload, recv)

		waitJSDone(t, js, 20*time.Second)
	})

	t.Run("go-dial-js-receive", func(t *testing.T) {
		payload := "hello-from-go"
		js := startJS(t, jsDir, "listen", relayAddr.String(), interopProtocol, payload)
		defer js.Kill()

		ready := waitJSReady(t, js, 20*time.Second)
		t.Logf("js webrtc addr: %s", ready)
		jsAddr, err := ma.NewMultiaddr(ready)
		require.NoError(t, err)
		jsAddr = ensureP2PAddr(jsAddr, "")

		jsPeer, err := peerIDFromAddr(jsAddr)
		require.NoError(t, err)
		goNode.Peerstore().AddAddrs(jsPeer, []ma.Multiaddr{jsAddr}, peerstore.TempAddrTTL)

		stream, err := goNode.NewStream(ctx, jsPeer, protocol.ID(interopProtocol))
		require.NoError(t, err)
		_, err = stream.Write([]byte(payload))
		require.NoError(t, err)
		require.NoError(t, stream.Close())

		waitJSDone(t, js, 20*time.Second)
	})
}

func startRelayHost(t *testing.T) (host.Host, peer.AddrInfo, ma.Multiaddr) {
	t.Helper()
	relay, err := libp2p.New(
		libp2p.Transport(websocket.New),
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0/ws"),
		libp2p.EnableRelayService(),
		libp2p.ForceReachabilityPublic(),
	)
	require.NoError(t, err)

	addrs := relay.Addrs()
	require.NotEmpty(t, addrs)

	info := peer.AddrInfo{ID: relay.ID(), Addrs: addrs}
	relayAddr := addrs[0].Encapsulate(ma.StringCast("/p2p/" + relay.ID().String()))
	return relay, info, relayAddr
}

func startWebRTCNode(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(
		libp2p.Transport(websocket.New),
		libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0/ws"),
		libp2p.EnableRelay(),
		libp2p.EnableWebRTCTransport(),
	)
	require.NoError(t, err)
	return h
}

func waitForWebRTCAddr(t *testing.T, h host.Host, timeout time.Duration) ma.Multiaddr {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, addr := range h.Addrs() {
			if hasProtocol(addr, ma.P_WEBRTC) {
				return addr
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for webrtc addr")
	return nil
}

func hasProtocol(addr ma.Multiaddr, code int) bool {
	for _, c := range addr {
		if c.Protocol().Code == code {
			return true
		}
	}
	return false
}

func ensureP2PAddr(addr ma.Multiaddr, id peer.ID) ma.Multiaddr {
	if addr == nil {
		return nil
	}
	if _, err := addr.ValueForProtocol(ma.P_P2P); err == nil {
		return addr
	}
	if id == "" {
		return addr
	}
	return addr.Encapsulate(ma.StringCast("/p2p/" + id.String()))
}

func buildRelayWebRTCAddr(relayAddr ma.Multiaddr, id peer.ID) ma.Multiaddr {
	if relayAddr == nil {
		return nil
	}
	return ma.StringCast(relayAddr.String() + "/p2p-circuit/webrtc/p2p/" + id.String())
}

func peerIDFromAddr(addr ma.Multiaddr) (peer.ID, error) {
	var idStr string
	for _, c := range addr {
		if c.Protocol().Code == ma.P_P2P {
			idStr = c.Value()
		}
	}
	if idStr == "" {
		return "", fmt.Errorf("missing peer id in addr %s", addr.String())
	}
	return peer.Decode(idStr)
}

func startJS(t *testing.T, dir string, args ...string) *jsProc {
	t.Helper()
	cmd := exec.Command("node", append([]string{"interop.js"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DEBUG=libp2p:webrtc*,libp2p:circuit-relay*")

	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stderrPipe, err := cmd.StderrPipe()
	require.NoError(t, err)

	proc := &jsProc{
		cmd:      cmd,
		events:   make(chan jsEvent, 16),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
		doneOnce: make(chan struct{}),
	}

	require.NoError(t, cmd.Start())

	go proc.readStdout(stdoutPipe)
	go proc.readStderr(stderrPipe)
	go func() {
		_ = cmd.Wait()
		close(proc.doneOnce)
	}()

	return proc
}

func (p *jsProc) readStdout(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		p.stdout.WriteString(line + "\n")
		var evt jsEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}
		evt.Raw = line
		p.events <- evt
	}
}

func (p *jsProc) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		p.stderr.WriteString(scanner.Text() + "\n")
	}
}

func (p *jsProc) Kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
}

func waitJSReady(t *testing.T, proc *jsProc, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-proc.events:
			if evt.Type == "ready" && evt.WebRTCAddr != "" {
				return evt.WebRTCAddr
			}
			if evt.Type == "done" && !evt.OK {
				t.Fatalf("js failed before ready: %s (stdout=%s stderr=%s)", evt.Error, proc.stdout.String(), proc.stderr.String())
			}
		case <-deadline:
			t.Fatalf("timed out waiting for js ready. stdout=%s stderr=%s", proc.stdout.String(), proc.stderr.String())
		}
	}
}

func waitJSDone(t *testing.T, proc *jsProc, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case evt := <-proc.events:
			if evt.Type == "done" {
				if !evt.OK {
					t.Fatalf("js failed: %s (stdout=%s stderr=%s)", evt.Error, proc.stdout.String(), proc.stderr.String())
				}
				return
			}
		case <-proc.doneOnce:
			if strings.Contains(proc.stdout.String(), `"type":"done"`) {
				return
			}
			t.Fatalf("js exited without done event. stdout=%s stderr=%s", proc.stdout.String(), proc.stderr.String())
		case <-deadline:
			t.Fatalf("timed out waiting for js done. stdout=%s stderr=%s", proc.stdout.String(), proc.stderr.String())
		}
	}
}

func waitStringWithJS(t *testing.T, ch <-chan string, proc *jsProc, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case value := <-ch:
			return value
		case evt := <-proc.events:
			if evt.Type == "done" && !evt.OK {
				t.Fatalf("js failed: %s (stdout=%s stderr=%s)", evt.Error, proc.stdout.String(), proc.stderr.String())
			}
		case <-deadline:
			t.Fatalf("timed out waiting for payload. stdout=%s stderr=%s", proc.stdout.String(), proc.stderr.String())
		}
	}
}
func waitString(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for payload")
	}
	return ""
}
