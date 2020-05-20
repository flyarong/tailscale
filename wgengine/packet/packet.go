// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package packet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
)

type IPProto int

const (
	Junk IPProto = iota
	Fragment
	ICMP
	UDP
	TCP
)

const (
	IPHeaderSize   = 20
	ICMPHeaderSize = 4
	UDPHeaderSize  = 8
)

const (
	ICMPDataOffset = IPHeaderSize + ICMPHeaderSize
	UDPDataOffset  = IPHeaderSize + UDPHeaderSize
)

const (
	icmpProtoId byte = 0x01
	udpProtoId       = 0x11
)

var ErrTooSmall = errors.New("packet too small")
var ErrTooLarge = errors.New("packet too large")

// RFC1858: prevent overlapping fragment attacks.
const MIN_FRAG = 60 + 20 // max IPv4 header + basic TCP header

func (p IPProto) String() string {
	switch p {
	case Fragment:
		return "Frag"
	case ICMP:
		return "ICMP"
	case UDP:
		return "UDP"
	case TCP:
		return "TCP"
	default:
		return "Junk"
	}
}

type IP uint32

func NewIP(b net.IP) IP {
	b4 := b.To4()
	if b4 == nil {
		panic(fmt.Sprintf("To4(%v) failed", b))
	}
	return IP(binary.BigEndian.Uint32(b4))
}

func (ip IP) String() string {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(ip))
	return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
}

const (
	EchoReply    uint8 = 0x00
	EchoRequest  uint8 = 0x08
	Unreachable  uint8 = 0x03
	TimeExceeded uint8 = 0x0B
)

const (
	TCPSyn    uint8 = 0x02
	TCPAck    uint8 = 0x10
	TCPSynAck uint8 = TCPSyn | TCPAck
)

type QDecode struct {
	b      []byte // Packet buffer that this decodes
	subofs int    // byte offset of IP subprotocol

	IPProto  IPProto // IP subprotocol (UDP, TCP, etc)
	SrcIP    IP      // IP source address
	DstIP    IP      // IP destination address
	SrcPort  uint16  // TCP/UDP source port
	DstPort  uint16  // TCP/UDP destination port
	TCPFlags uint8   // TCP flags (SYN, ACK, etc)
}

func (q QDecode) String() string {
	if q.IPProto == Junk {
		return "Junk{}"
	}
	srcip := make([]byte, 4)
	dstip := make([]byte, 4)
	binary.BigEndian.PutUint32(srcip, uint32(q.SrcIP))
	binary.BigEndian.PutUint32(dstip, uint32(q.DstIP))
	return fmt.Sprintf("%v{%d.%d.%d.%d:%d > %d.%d.%d.%d:%d}",
		q.IPProto,
		srcip[0], srcip[1], srcip[2], srcip[3], q.SrcPort,
		dstip[0], dstip[1], dstip[2], dstip[3], q.DstPort)
}

// based on https://tools.ietf.org/html/rfc1071
func ipChecksum(b []byte) uint16 {
	var ac uint32
	i := 0
	n := len(b)
	for n >= 2 {
		ac += uint32(binary.BigEndian.Uint16(b[i : i+2]))
		n -= 2
		i += 2
	}
	if n == 1 {
		ac += uint32(b[i]) << 8
	}
	for (ac >> 16) > 0 {
		ac = (ac >> 16) + (ac & 0xffff)
	}
	return uint16(^ac)
}

func writeIPHeader(srcIP, dstIP IP, ipID uint16, proto byte, out []byte) {
	size := len(out)
	// This is necessary because caller (e.g. WriteUDPChecksum)
	// may have used IP header space as scrap for a pseudo header.
	out[0] = 0x40 | (IPHeaderSize >> 2) // IPv4
	out[1] = 0x00                       // DHCP, ECN
	binary.BigEndian.PutUint16(out[2:4], uint16(size))
	binary.BigEndian.PutUint16(out[4:6], ipID)
	binary.BigEndian.PutUint16(out[6:8], 0) // flags, offset
	out[8] = 64                             // TTL
	out[9] = proto
	binary.BigEndian.PutUint16(out[10:12], 0) // blank IP header checksum
	binary.BigEndian.PutUint32(out[12:16], uint32(srcIP))
	binary.BigEndian.PutUint32(out[16:20], uint32(dstIP))

	binary.BigEndian.PutUint16(out[10:12], ipChecksum(out[0:20]))
}

func WriteUDPHeader(srcIP, dstIP IP, ipID uint16, srcPort, dstPort uint16, out []byte) error {
	if len(out) < UDPDataOffset {
		return ErrTooSmall
	}
	if len(out) > 65535 {
		return ErrTooLarge
	}

	udpSize := len(out[IPHeaderSize:]) // skip IP header space
	// IP pseudo header
	binary.BigEndian.PutUint32(out[8:12], uint32(srcIP))
	binary.BigEndian.PutUint32(out[12:16], uint32(dstIP))
	out[16] = 0x0
	out[17] = udpProtoId
	binary.BigEndian.PutUint16(out[18:20], uint16(udpSize))
	// UDP Header
	binary.BigEndian.PutUint16(out[20:22], srcPort)
	binary.BigEndian.PutUint16(out[22:24], dstPort)
	binary.BigEndian.PutUint16(out[24:26], uint16(udpSize))
	binary.BigEndian.PutUint16(out[26:28], 0) // blank UDP header checksum
	binary.BigEndian.PutUint16(out[26:28], ipChecksum(out[8:]))

	writeIPHeader(srcIP, dstIP, ipID, udpProtoId, out)
	return nil
}

func WriteICMPHeader(srcIP, dstIP IP, ipID uint16, icmpType, icmpCode byte, out []byte) error {
	if len(out) < ICMPDataOffset {
		return ErrTooSmall
	}
	if len(out) > 65535 {
		return ErrTooLarge
	}

	out[20] = icmpType
	out[21] = icmpCode
	binary.BigEndian.PutUint16(out[22:24], 0) // blank ICMP checksum

	writeIPHeader(srcIP, dstIP, ipID, icmpProtoId, out)

	binary.BigEndian.PutUint16(out[22:24], ipChecksum(out))
	return nil
}

func GenICMP(srcIP, dstIP IP, ipID uint16, icmpType uint8, icmpCode uint8, payload []byte) []byte {
	// Even though WriteICMPHeader also checks this,
	// do this early to prevent an unnecessary allocation.
	if len(payload) < 4 {
		return nil
	}
	if len(payload) > 65535-24 {
		return nil
	}

	sz := ICMPDataOffset + len(payload)
	out := make([]byte, sz)
	copy(out[ICMPDataOffset:], payload)

	WriteICMPHeader(srcIP, dstIP, ipID, icmpType, icmpCode, out)
	return out
}

// An extremely simple packet decoder for basic IPv4 packet types.
// It extracts only the subprotocol id, IP addresses, and (if any) ports,
// and shouldn't need any memory allocation.
func (q *QDecode) Decode(b []byte) {
	q.b = nil

	if len(b) < 20 {
		q.IPProto = Junk
		return
	}
	// Check that it's IPv4.
	// TODO(apenwarr): consider IPv6 support
	if ((b[0] & 0xF0) >> 4) != 4 {
		q.IPProto = Junk
		return
	}

	n := int(binary.BigEndian.Uint16(b[2:4]))
	if len(b) < n {
		// Packet was cut off before full IPv4 length.
		q.IPProto = Junk
		return
	}

	// If it's valid IPv4, then the IP addresses are valid
	q.SrcIP = IP(binary.BigEndian.Uint32(b[12:16]))
	q.DstIP = IP(binary.BigEndian.Uint32(b[16:20]))

	q.subofs = int((b[0] & 0x0F) * 4)
	sub := b[q.subofs:]

	// We don't care much about IP fragmentation, except insofar as it's
	// used for firewall bypass attacks. The trick is make the first
	// fragment of a TCP or UDP packet so short that it doesn't fit
	// the TCP or UDP header, so we can't read the port, in hope that
	// it'll sneak past. Then subsequent fragments fill it in, but we're
	// missing the first part of the header, so we can't read that either.
	//
	// A "perfectly correct" implementation would have to reassemble
	// fragments before deciding what to do. But the truth is there's
	// zero reason to send such a short first fragment, so we can treat
	// it as Junk. We can also treat any subsequent fragment that starts
	// at such a low offset as Junk.
	fragFlags := binary.BigEndian.Uint16(b[6:8])
	moreFrags := (fragFlags & 0x20) != 0
	fragOfs := fragFlags & 0x1FFF
	if fragOfs == 0 {
		// This is the first fragment
		if moreFrags && len(sub) < MIN_FRAG {
			// Suspiciously short first fragment, dump it.
			log.Printf("junk1!\n")
			q.IPProto = Junk
			return
		}
		// otherwise, this is either non-fragmented (the usual case)
		// or a big enough initial fragment that we can read the
		// whole subprotocol header.
		proto := b[9]
		switch proto {
		case 1: // ICMPv4
			if len(sub) < 8 {
				q.IPProto = Junk
				return
			}
			q.IPProto = ICMP
			q.SrcPort = 0
			q.DstPort = 0
			q.b = b
			return
		case 6: // TCP
			if len(sub) < 20 {
				q.IPProto = Junk
				return
			}
			q.IPProto = TCP
			q.SrcPort = binary.BigEndian.Uint16(sub[0:2])
			q.DstPort = binary.BigEndian.Uint16(sub[2:4])
			q.TCPFlags = sub[13] & 0x3F
			q.b = b
			return
		case 17: // UDP
			if len(sub) < 8 {
				q.IPProto = Junk
				return
			}
			q.IPProto = UDP
			q.SrcPort = binary.BigEndian.Uint16(sub[0:2])
			q.DstPort = binary.BigEndian.Uint16(sub[2:4])
			q.b = b
			return
		default:
			q.IPProto = Junk
			return
		}
	} else {
		// This is a fragment other than the first one.
		if fragOfs < MIN_FRAG {
			// First frag was suspiciously short, so we can't
			// trust the followup either.
			q.IPProto = Junk
			return
		}
		// otherwise, we have to permit the fragment to slide through.
		// Second and later fragments don't have sub-headers.
		// Ideally, we would drop fragments that we can't identify,
		// but that would require statefulness. Anyway, receivers'
		// kernels know to drop fragments where the initial fragment
		// doesn't arrive.
		q.IPProto = Fragment
		return
	}
}

// Returns a subset of the IP subprotocol section.
func (q *QDecode) Sub(begin, n int) []byte {
	return q.b[q.subofs+begin : q.subofs+begin+n]
}

// For a packet that is known to be IPv4, trim the buffer to its IPv4 length.
// Sometimes packets arrive from an interface with extra bytes on the end.
// This removes them.
func (q *QDecode) Trim() []byte {
	n := binary.BigEndian.Uint16(q.b[2:4])
	return q.b[0:n]
}

// For a decoded TCP packet, return true if it's a TCP SYN packet (ie. the
// first packet in a new connection).
func (q *QDecode) IsTCPSyn() bool {
	const Syn = 0x02
	const Ack = 0x10
	const SynAck = Syn | Ack
	return (q.TCPFlags & SynAck) == Syn
}

// For a packet that has already been decoded, check if it's an IPv4 ICMP
// "Error" packet.
func (q *QDecode) IsError() bool {
	if q.IPProto == ICMP && len(q.b) >= q.subofs+8 {
		switch q.b[q.subofs] {
		case Unreachable, TimeExceeded:
			return true
		}
	}
	return false
}

// For a packet that has already been decoded, check if it's an IPv4 ICMP
// Echo Request.
func (q *QDecode) IsEchoRequest() bool {
	if q.IPProto == ICMP && len(q.b) >= q.subofs+8 {
		return q.b[q.subofs] == EchoRequest && q.b[q.subofs+1] == 0
	}
	return false
}

// For a packet that has already been decoded, check if it's an IPv4 ICMP
// Echo Response.
func (q *QDecode) IsEchoResponse() bool {
	if q.IPProto == ICMP && len(q.b) >= q.subofs+8 {
		return q.b[q.subofs] == EchoReply && q.b[q.subofs+1] == 0
	}
	return false
}

func (q *QDecode) EchoRespond() []byte {
	icmpid := binary.BigEndian.Uint16(q.Sub(4, 2))
	b := q.Trim()
	return GenICMP(q.DstIP, q.SrcIP, icmpid, EchoReply, 0, b[q.subofs+4:])
}

func Hexdump(b []byte) string {
	out := new(strings.Builder)
	for i := 0; i < len(b); i += 16 {
		if i > 0 {
			fmt.Fprintf(out, "\n")
		}
		fmt.Fprintf(out, "  %04x  ", i)
		j := 0
		for ; j < 16 && i+j < len(b); j++ {
			if j == 8 {
				fmt.Fprintf(out, " ")
			}
			fmt.Fprintf(out, "%02x ", b[i+j])
		}
		for ; j < 16; j++ {
			if j == 8 {
				fmt.Fprintf(out, " ")
			}
			fmt.Fprintf(out, "   ")
		}
		fmt.Fprintf(out, " ")
		for j = 0; j < 16 && i+j < len(b); j++ {
			if b[i+j] >= 32 && b[i+j] < 128 {
				fmt.Fprintf(out, "%c", b[i+j])
			} else {
				fmt.Fprintf(out, ".")
			}
		}
	}
	return out.String()
}
