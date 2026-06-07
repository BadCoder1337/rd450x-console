package vmedia

import (
	"fmt"
	"strings"
)

// buildAuth assembles the IUSB session-token authentication packet for a device
// (a 32-byte header + 128-byte payload). It mirrors JViewer's
// CDROMRedir.SendAuth_SessionToken for the web-session-token case (token type 0):
//
//   - header: deviceType, Instance=instance, dataPacketLen=128, direction=128
//   - payload[opcodeOffset] = 0xF2 (auth opcode)
//   - payload[connStatusOffset] = 0
//   - payload[authTokenOffset..] = the web session token (STOKEN)
//
// token is the STOKEN minted by the web login (kvm.WebSession.Token).
func buildAuth(deviceType, instance uint8, token string) []byte {
	pkt := make([]byte, HeaderLen+authPayloadLen)
	h := Header{
		Major:         iusbMajor,
		Minor:         iusbMinor,
		DataPacketLen: authPayloadLen,
		DeviceType:    deviceType,
		Protocol:      1,
		Direction:     128,
		Instance:      instance,
	}
	h.marshal(pkt)

	payload := pkt[HeaderLen:]
	payload[opcodeOffset] = OpAuth
	payload[connStatusOffset] = 0
	copy(payload[authTokenOffset:], token)
	return pkt
}

// ackStatus interprets an ACK packet (opcode 0xF1) from the BMC. It returns nil
// when the redirection was accepted (connectionStatus == 1), or a descriptive
// error otherwise — including the owning client's IP when the device is already
// in use.
func ackStatus(p *Packet) error {
	if p.Opcode() != OpRedirectAck {
		return fmt.Errorf("vmedia: expected redirection ACK (0xF1), got opcode 0x%02X", p.Opcode())
	}
	if len(p.Payload) <= connStatusOffset {
		return fmt.Errorf("vmedia: ACK payload too short (%d bytes)", len(p.Payload))
	}
	switch status := int(p.Payload[connStatusOffset]); status {
	case connOK:
		return nil
	case connErrInUse5, connErrInUse8:
		return fmt.Errorf("vmedia: BMC rejected redirection (device error, status %d)", status)
	default:
		if ip := otherIP(p.Payload); ip != "" {
			return fmt.Errorf("vmedia: device already redirected by %s (status %d)", ip, status)
		}
		return fmt.Errorf("vmedia: redirection not accepted (status %d)", status)
	}
}

// otherIP extracts the NUL/space-trimmed owner IP string an ACK carries at
// authTokenOffset when the device is already in use (JViewer's m_otherIP).
func otherIP(payload []byte) string {
	if len(payload) <= authTokenOffset {
		return ""
	}
	raw := payload[authTokenOffset:]
	if i := indexByte(raw, 0); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(string(raw))
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}
