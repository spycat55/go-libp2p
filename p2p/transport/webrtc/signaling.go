package libp2pwebrtc

import (
	"errors"
	"fmt"
	"io"

	"github.com/libp2p/go-msgio"
	"google.golang.org/protobuf/encoding/protowire"
)

const signalingProtocol = "/webrtc-signaling/0.0.1"

const maxSignalingMessageSize = 1 << 20

type signalingMessageType int32

const (
	signalingMessageSDPOffer     signalingMessageType = 0
	signalingMessageSDPAnswer    signalingMessageType = 1
	signalingMessageIceCandidate signalingMessageType = 2
)

type signalingMessage struct {
	Type signalingMessageType
	Data string
}

type signalingStream struct {
	reader msgio.ReadCloser
	writer msgio.WriteCloser
}

func newSignalingStream(rw io.ReadWriter) *signalingStream {
	return &signalingStream{
		reader: msgio.NewVarintReaderSize(rw, maxSignalingMessageSize),
		writer: msgio.NewVarintWriter(rw),
	}
}

func (s *signalingStream) Read() (*signalingMessage, error) {
	buf, err := s.reader.ReadMsg()
	if err != nil {
		return nil, err
	}
	return decodeSignalingMessage(buf)
}

func (s *signalingStream) Write(msg *signalingMessage) error {
	encoded := encodeSignalingMessage(msg)
	return s.writer.WriteMsg(encoded)
}

func encodeSignalingMessage(msg *signalingMessage) []byte {
	var buf []byte
	buf = protowire.AppendTag(buf, 1, protowire.VarintType)
	buf = protowire.AppendVarint(buf, uint64(msg.Type))
	if msg.Data != "" {
		buf = protowire.AppendTag(buf, 2, protowire.BytesType)
		buf = protowire.AppendString(buf, msg.Data)
	}
	return buf
}

func decodeSignalingMessage(buf []byte) (*signalingMessage, error) {
	msg := &signalingMessage{}
	for len(buf) > 0 {
		num, typ, n := protowire.ConsumeTag(buf)
		if n < 0 {
			return nil, fmt.Errorf("invalid signaling message tag")
		}
		buf = buf[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				return nil, errors.New("invalid signaling message type field")
			}
			val, n := protowire.ConsumeVarint(buf)
			if n < 0 {
				return nil, errors.New("invalid signaling message type value")
			}
			msg.Type = signalingMessageType(val)
			buf = buf[n:]
		case 2:
			if typ != protowire.BytesType {
				return nil, errors.New("invalid signaling message data field")
			}
			val, n := protowire.ConsumeString(buf)
			if n < 0 {
				return nil, errors.New("invalid signaling message data value")
			}
			msg.Data = val
			buf = buf[n:]
		default:
			n := protowire.ConsumeFieldValue(num, typ, buf)
			if n < 0 {
				return nil, errors.New("invalid signaling message field")
			}
			buf = buf[n:]
		}
	}
	return msg, nil
}
