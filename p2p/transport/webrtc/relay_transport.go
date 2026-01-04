package libp2pwebrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/connmgr"
	ic "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/pnet"
	"github.com/libp2p/go-libp2p/core/transport"

	ma "github.com/multiformats/go-multiaddr"
	"github.com/pion/webrtc/v4"
)

var webrtcAddr = ma.StringCast("/webrtc")

// AddRelayTransport adds the WebRTC (non-direct) transport to the host network.
func AddRelayTransport(
	h host.Host,
	privKey ic.PrivKey,
	psk pnet.PSK,
	gater connmgr.ConnectionGater,
	rcmgr network.ResourceManager,
	listenUDP ListenUDPFn,
) error {
	n, ok := h.Network().(transport.TransportNetwork)
	if !ok {
		return fmt.Errorf("%v is not a transport network", h.Network())
	}

	base, err := New(privKey, psk, gater, rcmgr, listenUDP)
	if err != nil {
		return fmt.Errorf("error constructing webrtc transport: %w", err)
	}

	rt := &relayTransport{
		host:      h,
		base:      base,
		incoming:  make(chan transport.CapableConn, maxAcceptQueueLen),
		listenSet: make(chan struct{}),
	}

	if err := n.AddTransport(rt); err != nil {
		return fmt.Errorf("error adding webrtc transport: %w", err)
	}

	if err := n.Listen(webrtcAddr); err != nil {
		return fmt.Errorf("error listening on webrtc addr: %w", err)
	}

	return nil
}

type relayTransport struct {
	host     host.Host
	base     *WebRTCTransport
	incoming chan transport.CapableConn

	listenOnce sync.Once
	listenSet  chan struct{}

	closeOnce sync.Once
}

var _ transport.Transport = (*relayTransport)(nil)

func (t *relayTransport) CanDial(addr ma.Multiaddr) bool {
	for _, c := range addr {
		if c.Protocol().Code == ma.P_WEBRTC {
			return true
		}
	}
	return false
}

func (t *relayTransport) Listen(addr ma.Multiaddr) (transport.Listener, error) {
	if !isWebRTCAddr(addr) {
		return nil, fmt.Errorf("must listen on /webrtc multiaddr")
	}

	t.listenOnce.Do(func() {
		t.host.SetStreamHandler(signalingProtocol, t.handleSignalingStream)
		close(t.listenSet)
	})

	return &relayListener{
		transport: t,
		done:      make(chan struct{}),
	}, nil
}

func (t *relayTransport) Dial(ctx context.Context, addr ma.Multiaddr, p peer.ID) (transport.CapableConn, error) {
	if !t.CanDial(addr) {
		return nil, fmt.Errorf("unsupported multiaddr: %s", addr)
	}

	circuitAddr, targetPeer, err := splitWebRTCAddr(addr)
	if err != nil {
		return nil, err
	}
	if p != "" && p != targetPeer {
		return nil, fmt.Errorf("peer mismatch for webrtc dial: %s != %s", p, targetPeer)
	}
	if p == "" {
		p = targetPeer
	}

	scope, err := t.base.rcmgr.OpenConnection(network.DirOutbound, false, addr)
	if err != nil {
		return nil, err
	}
	if err := scope.SetPeer(p); err != nil {
		scope.Done()
		return nil, err
	}

	defer func() {
		if err != nil {
			scope.Done()
		}
	}()

	t.host.Peerstore().AddAddrs(p, []ma.Multiaddr{circuitAddr}, peerstore.TempAddrTTL)

	signalCtx := network.WithAllowLimitedConn(ctx, "webrtc-signaling")
	stream, err := t.host.NewStream(signalCtx, p, signalingProtocol)
	if err != nil {
		return nil, fmt.Errorf("open signaling stream: %w", err)
	}

	conn, err := t.connectAsInitiator(ctx, stream, p, scope)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func (t *relayTransport) Protocols() []int {
	return []int{ma.P_WEBRTC}
}

func (t *relayTransport) Proxy() bool {
	return false
}

func (t *relayTransport) handleSignalingStream(stream network.Stream) {
	<-t.listenSet

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	remotePeer := stream.Conn().RemotePeer()
	remoteAddr := webrtcAddrForPeer(remotePeer)

	scope, err := t.base.rcmgr.OpenConnection(network.DirInbound, false, remoteAddr)
	if err != nil {
		log.Debug("webrtc relay inbound scope error", "err", err)
		return
	}
	if err := scope.SetPeer(remotePeer); err != nil {
		scope.Done()
		log.Debug("webrtc relay inbound scope peer error", "err", err)
		return
	}

	conn, err := t.connectAsResponder(ctx, stream, remotePeer, scope)
	if err != nil {
		scope.Done()
		log.Debug("webrtc relay inbound handshake error", "err", err)
		return
	}

	select {
	case t.incoming <- conn:
	default:
		_ = conn.Close()
		log.Debug("webrtc relay incoming queue full")
	}
}

func (t *relayTransport) connectAsInitiator(
	ctx context.Context,
	stream network.Stream,
	remotePeer peer.ID,
	scope network.ConnManagementScope,
) (transport.CapableConn, error) {
	w, err := t.newRelayPeerConnection(scope)
	if err != nil {
		return nil, err
	}

	signal := newSignalingStream(stream)
	errC := addOnConnectionStateChangeCallback(w.PeerConnection)

	initChannel, err := w.PeerConnection.CreateDataChannel("init", nil)
	if err != nil {
		return nil, fmt.Errorf("create init channel: %w", err)
	}
	initClosed := make(chan struct{})
	initChannel.OnOpen(func() {
		_ = initChannel.Close()
	})
	initChannel.OnClose(func() {
		select {
		case <-initClosed:
		default:
			close(initClosed)
		}
	})

	w.PeerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if w.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if candidateJSON.SDPMid == nil {
			sdpMid := "0"
			candidateJSON.SDPMid = &sdpMid
		}
		if candidateJSON.SDPMLineIndex == nil {
			sdpLine := uint16(0)
			candidateJSON.SDPMLineIndex = &sdpLine
		}
		if !strings.HasPrefix(candidateJSON.Candidate, "a=") {
			candidateJSON.Candidate = "a=" + candidateJSON.Candidate
		}
		log.Debug("webrtc relay send candidate", "candidate", candidateJSON.Candidate)
		data, err := json.Marshal(candidateJSON)
		if err != nil {
			log.Debug("webrtc relay candidate marshal error", "err", err)
			return
		}
		_ = signal.Write(&signalingMessage{
			Type: signalingMessageIceCandidate,
			Data: string(data),
		})
	})

	offer, err := w.PeerConnection.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("create offer: %w", err)
	}

	if err := signal.Write(&signalingMessage{
		Type: signalingMessageSDPOffer,
		Data: offer.SDP,
	}); err != nil {
		return nil, fmt.Errorf("send sdp offer: %w", err)
	}

	if err := w.PeerConnection.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}

	answer, err := signal.Read()
	if err != nil {
		return nil, fmt.Errorf("read sdp answer: %w", err)
	}
	if answer.Type != signalingMessageSDPAnswer {
		return nil, fmt.Errorf("expected sdp answer, got %d", answer.Type)
	}

	if err := w.PeerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  answer.Data,
	}); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	if err := readCandidatesUntilConnected(ctx, w.PeerConnection, signal, errC); err != nil {
		return nil, err
	}

	select {
	case <-initClosed:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	remotePubKey := t.host.Peerstore().PubKey(remotePeer)

	localAddr := webrtcAddrForPeer(t.base.localPeerId)
	remoteAddr := webrtcAddrForPeer(remotePeer)

	conn, err := newConnection(
		network.DirOutbound,
		w.PeerConnection,
		t.base,
		scope,
		t.base.localPeerId,
		localAddr,
		remotePeer,
		remotePubKey,
		remoteAddr,
		w.IncomingDataChannels,
		w.PeerConnectionClosedCh,
	)
	if err != nil {
		return nil, err
	}

	if t.base.gater != nil && !t.base.gater.InterceptSecured(network.DirOutbound, remotePeer, conn) {
		return nil, errors.New("secured connection gated")
	}

	return conn, nil
}

func (t *relayTransport) connectAsResponder(
	ctx context.Context,
	stream network.Stream,
	remotePeer peer.ID,
	scope network.ConnManagementScope,
) (transport.CapableConn, error) {
	w, err := t.newRelayPeerConnection(scope)
	if err != nil {
		return nil, err
	}

	signal := newSignalingStream(stream)
	errC := addOnConnectionStateChangeCallback(w.PeerConnection)

	w.PeerConnection.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if w.PeerConnection.ConnectionState() == webrtc.PeerConnectionStateConnected {
			return
		}
		if candidate == nil {
			return
		}
		candidateJSON := candidate.ToJSON()
		if candidateJSON.SDPMid == nil {
			sdpMid := "0"
			candidateJSON.SDPMid = &sdpMid
		}
		if candidateJSON.SDPMLineIndex == nil {
			sdpLine := uint16(0)
			candidateJSON.SDPMLineIndex = &sdpLine
		}
		if !strings.HasPrefix(candidateJSON.Candidate, "a=") {
			candidateJSON.Candidate = "a=" + candidateJSON.Candidate
		}
		log.Debug("webrtc relay send candidate", "candidate", candidateJSON.Candidate)
		data, err := json.Marshal(candidateJSON)
		if err != nil {
			log.Debug("webrtc relay candidate marshal error", "err", err)
			return
		}
		_ = signal.Write(&signalingMessage{
			Type: signalingMessageIceCandidate,
			Data: string(data),
		})
	})

	offer, err := signal.Read()
	if err != nil {
		return nil, fmt.Errorf("read sdp offer: %w", err)
	}
	if offer.Type != signalingMessageSDPOffer {
		return nil, fmt.Errorf("expected sdp offer, got %d", offer.Type)
	}

	if err := w.PeerConnection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  offer.Data,
	}); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	answer, err := w.PeerConnection.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create answer: %w", err)
	}

	if err := signal.Write(&signalingMessage{
		Type: signalingMessageSDPAnswer,
		Data: answer.SDP,
	}); err != nil {
		return nil, fmt.Errorf("send sdp answer: %w", err)
	}

	if err := w.PeerConnection.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}

	if err := readCandidatesUntilConnected(ctx, w.PeerConnection, signal, errC); err != nil {
		return nil, err
	}

	remotePubKey := t.host.Peerstore().PubKey(remotePeer)

	localAddr := webrtcAddrForPeer(t.base.localPeerId)
	remoteAddr := webrtcAddrForPeer(remotePeer)

	conn, err := newConnection(
		network.DirInbound,
		w.PeerConnection,
		t.base,
		scope,
		t.base.localPeerId,
		localAddr,
		remotePeer,
		remotePubKey,
		remoteAddr,
		w.IncomingDataChannels,
		w.PeerConnectionClosedCh,
	)
	if err != nil {
		return nil, err
	}

	if t.base.gater != nil && !t.base.gater.InterceptSecured(network.DirInbound, remotePeer, conn) {
		return nil, errors.New("secured connection gated")
	}

	return conn, nil
}

func (t *relayTransport) newRelayPeerConnection(scope network.ResourceScopeSpan) (webRTCConnection, error) {
	settingEngine := webrtc.SettingEngine{
		LoggerFactory: pionLoggerFactory,
	}
	settingEngine.DetachDataChannels()
	settingEngine.SetPrflxAcceptanceMinWait(0)
	settingEngine.SetICETimeouts(
		t.base.peerConnectionTimeouts.Disconnect,
		t.base.peerConnectionTimeouts.Failed,
		t.base.peerConnectionTimeouts.Keepalive,
	)
	settingEngine.SetIncludeLoopbackCandidate(true)
	settingEngine.SetSCTPMaxReceiveBufferSize(sctpReceiveBufferSize)

	if err := scope.ReserveMemory(sctpReceiveBufferSize, network.ReservationPriorityMedium); err != nil {
		return webRTCConnection{}, err
	}

	return newWebRTCConnectionWithHandshake(settingEngine, t.base.webrtcConfig, false)
}

func readCandidatesUntilConnected(
	ctx context.Context,
	pc *webrtc.PeerConnection,
	signal *signalingStream,
	errC <-chan error,
) error {
	type readResult struct {
		msg *signalingMessage
		err error
	}

	msgCh := make(chan readResult, 1)
	stopCh := make(chan struct{})
	go func() {
		defer close(msgCh)
		for {
			msg, err := signal.Read()
			select {
			case msgCh <- readResult{msg: msg, err: err}:
			case <-stopCh:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer close(stopCh)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, ok := <-errC:
			if !ok {
				return nil
			}
			return err
		case result, ok := <-msgCh:
			if !ok {
				return errors.New("signaling stream closed")
			}
			if result.err != nil {
				return result.err
			}
			if result.msg == nil || result.msg.Type != signalingMessageIceCandidate {
				return errors.New("unexpected signaling message")
			}
			if result.msg.Data == "" || result.msg.Data == "null" {
				continue
			}
			var candidateInit webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(result.msg.Data), &candidateInit); err != nil {
				log.Debug("webrtc relay candidate parse error", "err", err)
				continue
			}
			if strings.HasPrefix(candidateInit.Candidate, "a=") {
				candidateInit.Candidate = strings.TrimPrefix(candidateInit.Candidate, "a=")
			}
			log.Debug("webrtc relay add candidate", "candidate", candidateInit.Candidate)
			if err := pc.AddICECandidate(candidateInit); err != nil {
				log.Debug("webrtc relay add candidate error", "err", err)
			}
		}
	}
}

func webrtcAddrForPeer(id peer.ID) ma.Multiaddr {
	return ma.StringCast(fmt.Sprintf("/webrtc/p2p/%s", id.String()))
}

func splitWebRTCAddr(addr ma.Multiaddr) (ma.Multiaddr, peer.ID, error) {
	var target peer.ID
	for _, c := range addr {
		if c.Protocol().Code == ma.P_P2P {
			id, err := peer.Decode(c.Value())
			if err != nil {
				return nil, "", err
			}
			target = id
		}
	}
	if target == "" {
		return nil, "", fmt.Errorf("webrtc target peer id missing")
	}

	components := make([]ma.Component, 0, len(addr))
	for _, c := range addr {
		if c.Protocol().Code == ma.P_WEBRTC {
			continue
		}
		components = append(components, c)
	}
	if len(components) == 0 {
		return nil, "", fmt.Errorf("webrtc circuit address missing")
	}

	return ma.Multiaddr(components), target, nil
}

func isWebRTCAddr(addr ma.Multiaddr) bool {
	for _, c := range addr {
		if c.Protocol().Code == ma.P_WEBRTC {
			return true
		}
	}
	return false
}

type relayListener struct {
	transport *relayTransport
	done      chan struct{}
	closeOnce sync.Once
}

func (l *relayListener) Accept() (transport.CapableConn, error) {
	select {
	case conn := <-l.transport.incoming:
		return conn, nil
	case <-l.done:
		return nil, transport.ErrListenerClosed
	}
}

func (l *relayListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.done)
	})
	return nil
}

func (l *relayListener) Addr() net.Addr {
	return webrtcNetAddr{}
}

func (l *relayListener) Multiaddr() ma.Multiaddr {
	return webrtcAddr
}

type webrtcNetAddr struct{}

func (webrtcNetAddr) Network() string { return "webrtc" }
func (webrtcNetAddr) String() string  { return "webrtc" }
