//go:build webssh

package webssh

import "fmt"

// Message types. Wire-compatible with mole's webssh tunnel protocol
// (mole server/internal/api/webssh_handler.go) so the mole admin UI can
// connect to this endpoint unchanged in the future.
const (
	msgData   byte = 0x01 // terminal payload
	msgResize byte = 0x02 // {"cols":..,"rows":..}
	msgKeep   byte = 0x03 // keepalive; receiver ignores

	msgFileListReq     byte = 0x04 // {"path"}
	msgFileUploadReq   byte = 0x05 // {"path","size"}
	msgFileUploadData  byte = 0x06 // raw chunk (<=32KiB)
	msgFileUploadEnd   byte = 0x07 // no payload
	msgFileDownloadReq byte = 0x08 // {"path"}
	msgFileDeleteReq   byte = 0x09 // {"path"}
	msgFileMkdirReq    byte = 0x0A // {"path"}
	msgFileReadReq     byte = 0x0B // {"path"} — like download, ack has mode:"read"

	msgFileListResp byte = 0x0C // {"path","home","files":[..],"truncated"?}
	msgFileDataResp byte = 0x0D // raw chunk (download/read stream)
	msgFileAckResp  byte = 0x0E // {"ok","msg","size"?,"name"?,"mode"?}
	msgFileReadResp byte = 0x0F // reserved; constant kept for mole parity
)

// validTypes lists every accepted message type byte.
var validTypes = map[byte]bool{
	msgData: true, msgResize: true, msgKeep: true,
	msgFileListReq: true, msgFileUploadReq: true, msgFileUploadData: true,
	msgFileUploadEnd: true, msgFileDownloadReq: true, msgFileDeleteReq: true,
	msgFileMkdirReq: true, msgFileReadReq: true,
	msgFileListResp: true, msgFileDataResp: true, msgFileAckResp: true, msgFileReadResp: true,
}

// encodeFrame packs one WS binary message: [type 1B][payload].
func encodeFrame(typ byte, payload []byte) []byte {
	frame := make([]byte, 1+len(payload))
	frame[0] = typ
	copy(frame[1:], payload)
	return frame
}

// decodeFrame splits one WS binary message. Empty frames and unknown
// message types are rejected.
func decodeFrame(frame []byte) (byte, []byte, error) {
	if len(frame) == 0 {
		return 0, nil, fmt.Errorf("webssh: empty frame")
	}
	typ := frame[0]
	if !validTypes[typ] {
		return 0, nil, fmt.Errorf("webssh: unknown frame type %#x", typ)
	}
	return typ, frame[1:], nil
}
