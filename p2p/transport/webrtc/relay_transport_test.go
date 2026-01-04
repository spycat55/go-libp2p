package libp2pwebrtc

import (
	"fmt"
	"testing"

	coretest "github.com/libp2p/go-libp2p/core/test"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/require"
)

func TestSplitWebRTCAddr(t *testing.T) {
	relay := coretest.RandPeerIDFatal(t)
	dest := coretest.RandPeerIDFatal(t)

	addr := ma.StringCast(fmt.Sprintf(
		"/ip4/1.2.3.4/tcp/1234/ws/p2p/%s/p2p-circuit/webrtc/p2p/%s",
		relay.String(),
		dest.String(),
	))

	circuitAddr, peerID, err := splitWebRTCAddr(addr)
	require.NoError(t, err)
	require.Equal(t, dest, peerID)
	require.Equal(t,
		fmt.Sprintf("/ip4/1.2.3.4/tcp/1234/ws/p2p/%s/p2p-circuit/p2p/%s", relay.String(), dest.String()),
		circuitAddr.String(),
	)
}

func TestSignalingMessageRoundTrip(t *testing.T) {
	msg := &signalingMessage{
		Type: signalingMessageIceCandidate,
		Data: `{"candidate":"candidate:0 1 UDP 2122252543 192.168.0.1 12345 typ host"}`,
	}

	encoded := encodeSignalingMessage(msg)
	decoded, err := decodeSignalingMessage(encoded)
	require.NoError(t, err)
	require.Equal(t, msg.Type, decoded.Type)
	require.Equal(t, msg.Data, decoded.Data)
}
