// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Hubble

package threefour

import (
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"go4.org/netipx"
	"google.golang.org/protobuf/types/known/wrapperspb"

	pb "github.com/cilium/cilium/api/v1/flow"
	"github.com/cilium/cilium/pkg/byteorder"
	"github.com/cilium/cilium/pkg/hubble/parser/common"
	"github.com/cilium/cilium/pkg/hubble/parser/errors"
	"github.com/cilium/cilium/pkg/hubble/parser/getters"
	"github.com/cilium/cilium/pkg/hubble/parser/options"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/monitor"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"
	"github.com/cilium/cilium/pkg/policy/correlation"
)

// Parser is a parser for L3/L4 payloads
type Parser struct {
	log            *slog.Logger
	endpointGetter getters.EndpointGetter
	identityGetter getters.IdentityGetter
	dnsGetter      getters.DNSGetter
	ipGetter       getters.IPGetter
	serviceGetter  getters.ServiceGetter
	linkGetter     getters.LinkGetter

	epResolver          *common.EndpointResolver
	correlateL3L4Policy bool

	// TODO: consider using a pool of these
	packet *packet
}

// re-usable packet to avoid reallocating gopacket datastructures
type packet struct {
	lock.Mutex

	decLayerL2Dev *gopacket.DecodingLayerParser
	decLayerL3Dev struct {
		IPv4 *gopacket.DecodingLayerParser
		IPv6 *gopacket.DecodingLayerParser
	}
	decLayerOverlay struct {
		VXLAN  *gopacket.DecodingLayerParser
		Geneve *gopacket.DecodingLayerParser
	}

	Layers []gopacket.LayerType
	layers.Ethernet
	layers.IPv4
	layers.IPv6
	layers.ICMPv4
	layers.ICMPv6
	layers.TCP
	layers.UDP
	layers.SCTP

	overlay struct {
		Layers []gopacket.LayerType
		layers.VXLAN
		layers.Geneve
		layers.Ethernet
		layers.IPv4
		layers.IPv6
		layers.ICMPv4
		layers.ICMPv6
		layers.TCP
		layers.UDP
		layers.SCTP
	}
}

// New returns a new L3/L4 parser
func New(
	log *slog.Logger,
	endpointGetter getters.EndpointGetter,
	identityGetter getters.IdentityGetter,
	dnsGetter getters.DNSGetter,
	ipGetter getters.IPGetter,
	serviceGetter getters.ServiceGetter,
	linkGetter getters.LinkGetter,
	opts ...options.Option,
) (*Parser, error) {
	packet := &packet{}
	decoders := []gopacket.DecodingLayer{
		&packet.Ethernet,
		&packet.IPv4, &packet.IPv6,
		&packet.ICMPv4, &packet.ICMPv6,
		&packet.TCP, &packet.UDP, &packet.SCTP,
	}
	packet.decLayerL2Dev = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet, decoders...)
	packet.decLayerL3Dev.IPv4 = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv4, decoders...)
	packet.decLayerL3Dev.IPv6 = gopacket.NewDecodingLayerParser(layers.LayerTypeIPv6, decoders...)

	overlayDecoders := []gopacket.DecodingLayer{
		&packet.overlay.VXLAN, &packet.overlay.Geneve,
		&packet.overlay.Ethernet,
		&packet.overlay.IPv4, &packet.overlay.IPv6,
		&packet.overlay.ICMPv4, &packet.overlay.ICMPv6,
		&packet.overlay.TCP, &packet.overlay.UDP, &packet.overlay.SCTP,
	}
	packet.decLayerOverlay.VXLAN = gopacket.NewDecodingLayerParser(layers.LayerTypeVXLAN, overlayDecoders...)
	packet.decLayerOverlay.Geneve = gopacket.NewDecodingLayerParser(layers.LayerTypeGeneve, overlayDecoders...)
	// Let packet.decLayer.DecodeLayers return a nil error when it
	// encounters a layer it doesn't have a parser for, instead of returning
	// an UnsupportedLayerType error.
	packet.decLayerL2Dev.IgnoreUnsupported = true
	packet.decLayerL3Dev.IPv4.IgnoreUnsupported = true
	packet.decLayerL3Dev.IPv6.IgnoreUnsupported = true
	packet.decLayerOverlay.VXLAN.IgnoreUnsupported = true
	packet.decLayerOverlay.Geneve.IgnoreUnsupported = true

	args := &options.Options{
		EnableNetworkPolicyCorrelation: true,
	}

	for _, opt := range opts {
		opt(args)
	}

	return &Parser{
		log:                 log,
		dnsGetter:           dnsGetter,
		endpointGetter:      endpointGetter,
		identityGetter:      identityGetter,
		ipGetter:            ipGetter,
		serviceGetter:       serviceGetter,
		linkGetter:          linkGetter,
		epResolver:          common.NewEndpointResolver(log, endpointGetter, identityGetter, ipGetter),
		packet:              packet,
		correlateL3L4Policy: args.EnableNetworkPolicyCorrelation,
	}, nil
}

// Decode decodes the data from 'data' into 'decoded'
func (p *Parser) Decode(data []byte, decoded *pb.Flow) error {
	if len(data) == 0 {
		return errors.ErrEmptyData
	}

	eventType := data[0]

	var packetOffset int
	var dn *monitor.DropNotify
	var tn *monitor.TraceNotify
	var pvn *monitor.PolicyVerdictNotify
	var dbg *monitor.DebugCapture
	var eventSubType uint8
	var authType pb.AuthType

	switch eventType {
	case monitorAPI.MessageTypeDrop:
		dn = &monitor.DropNotify{}
		if err := dn.Decode(data); err != nil {
			return fmt.Errorf("failed to parse drop: %w", err)
		}
		eventSubType = dn.SubType
		packetOffset = (int)(dn.DataOffset())
	case monitorAPI.MessageTypeTrace:
		tn = &monitor.TraceNotify{}
		if err := tn.Decode(data); err != nil {
			return fmt.Errorf("failed to parse trace: %w", err)
		}
		eventSubType = tn.ObsPoint

		if tn.ObsPoint != 0 {
			decoded.TraceObservationPoint = pb.TraceObservationPoint(tn.ObsPoint)
		} else {
			// specifically handle the zero value in the observation enum so the json
			// export and the API don't carry extra meaning with the zero value
			decoded.TraceObservationPoint = pb.TraceObservationPoint_TO_ENDPOINT
		}

		packetOffset = (int)(tn.DataOffset())
	case monitorAPI.MessageTypePolicyVerdict:
		pvn = &monitor.PolicyVerdictNotify{}
		if err := pvn.Decode(data); err != nil {
			return fmt.Errorf("failed to parse policy verdict: %w", err)
		}
		eventSubType = pvn.SubType
		packetOffset = monitor.PolicyVerdictNotifyLen
		authType = pb.AuthType(pvn.GetAuthType())
	case monitorAPI.MessageTypeCapture:
		dbg = &monitor.DebugCapture{}
		if err := dbg.Decode(data); err != nil {
			return fmt.Errorf("failed to parse debug capture: %w", err)
		}
		eventSubType = dbg.SubType
		packetOffset = monitor.DebugCaptureLen
	default:
		return errors.NewErrInvalidType(eventType)
	}

	if len(data) < packetOffset {
		return fmt.Errorf("not enough bytes to decode %d", data)
	}

	isL3Device := tn != nil && tn.IsL3Device() || dn != nil && dn.IsL3Device()
	isIPv6 := tn != nil && tn.IsIPv6() || dn != nil && dn.IsIPv6()
	isVXLAN := tn != nil && tn.IsVXLAN() || dn != nil && dn.IsVXLAN()
	isGeneve := tn != nil && tn.IsGeneve() || dn != nil && dn.IsGeneve()
	ether, ip, l4, tunnel, srcIP, dstIP, srcPort, dstPort, summary, err := decodeLayers(data[packetOffset:], p.packet, isL3Device, isIPv6, isVXLAN, isGeneve)
	if err != nil {
		return err
	}

	if tn != nil && ip != nil {
		if !tn.OriginalIP().IsUnspecified() {
			// Ignore invalid IP - getters will handle invalid value.
			srcIP, _ = netipx.FromStdIP(tn.OriginalIP())
			// On SNAT the trace notification has OrigIP set to the pre
			// translation IP and the source IP parsed from the header is the
			// post translation IP. The check is here because sometimes we get
			// trace notifications with OrigIP set to the header's IP
			// (pre-translation events?)
			if ip.GetSource() != srcIP.String() {
				ip.SourceXlated = ip.GetSource()
				ip.Source = srcIP.String()
			}
		}

		ip.Encrypted = tn.IsEncrypted()
	}

	srcLabelID, dstLabelID := decodeSecurityIdentities(dn, tn, pvn)
	datapathContext := common.DatapathContext{
		SrcIP:                 srcIP,
		SrcLabelID:            srcLabelID,
		DstIP:                 dstIP,
		DstLabelID:            dstLabelID,
		TraceObservationPoint: decoded.TraceObservationPoint,
	}
	srcEndpoint := p.epResolver.ResolveEndpoint(srcIP, srcLabelID, datapathContext)
	dstEndpoint := p.epResolver.ResolveEndpoint(dstIP, dstLabelID, datapathContext)
	var sourceService, destinationService *pb.Service
	if p.serviceGetter != nil {
		sourceService = p.serviceGetter.GetServiceByAddr(srcIP, srcPort)
		destinationService = p.serviceGetter.GetServiceByAddr(dstIP, dstPort)
	}

	decoded.Verdict = decodeVerdict(dn, tn, pvn)
	decoded.AuthType = authType
	decoded.DropReason = decodeDropReason(dn, pvn)
	decoded.DropReasonDesc = pb.DropReason(decoded.DropReason)
	decoded.File = decodeFileInfo(dn)
	decoded.Ethernet = ether
	decoded.IP = ip
	decoded.L4 = l4
	decoded.Tunnel = tunnel
	decoded.Source = srcEndpoint
	decoded.Destination = dstEndpoint
	decoded.Type = pb.FlowType_L3_L4
	decoded.SourceNames = p.resolveNames(dstEndpoint.ID, srcIP)
	decoded.DestinationNames = p.resolveNames(srcEndpoint.ID, dstIP)
	decoded.L7 = nil
	decoded.IsReply = decodeIsReply(tn, pvn)
	decoded.Reply = decoded.GetIsReply().GetValue() // false if GetIsReply() is nil
	decoded.TrafficDirection = decodeTrafficDirection(srcEndpoint.ID, dn, tn, pvn)
	decoded.EventType = decodeCiliumEventType(eventType, eventSubType)
	decoded.TraceReason = decodeTraceReason(tn)
	decoded.SourceService = sourceService
	decoded.DestinationService = destinationService
	decoded.PolicyMatchType = decodePolicyMatchType(pvn)
	decoded.DebugCapturePoint = decodeDebugCapturePoint(dbg)
	decoded.Interface = p.decodeNetworkInterface(tn, dbg)
	decoded.ProxyPort = decodeProxyPort(dbg, tn)
	decoded.Summary = summary

	if p.correlateL3L4Policy && p.endpointGetter != nil {
		correlation.CorrelatePolicy(p.log, p.endpointGetter, decoded)
	}

	return nil
}

func (p *Parser) resolveNames(epID uint32, ip netip.Addr) (names []string) {
	if p.dnsGetter != nil {
		return p.dnsGetter.GetNamesOf(epID, ip)
	}

	return nil
}

func decodeLayers(payload []byte, packet *packet, isL3Device, isIPv6, isVXLAN, isGeneve bool) (
	ethernet *pb.Ethernet,
	ip *pb.IP,
	l4 *pb.Layer4,
	tunnel *pb.Tunnel,
	sourceIP, destinationIP netip.Addr,
	sourcePort, destinationPort uint16,
	summary string,
	err error,
) {
	packet.Lock()
	defer packet.Unlock()

	// Since v1.1.18, DecodeLayers returns a non-nil error for an empty packet, see
	// https://github.com/google/gopacket/issues/846
	// TODO: reconsider this check if the issue is fixed upstream
	if len(payload) == 0 {
		// Truncate layers to avoid accidental re-use.
		packet.Layers = packet.Layers[:0]
		packet.overlay.Layers = packet.overlay.Layers[:0]
		return
	}

	switch {
	case !isL3Device:
		err = packet.decLayerL2Dev.DecodeLayers(payload, &packet.Layers)
	case isIPv6:
		err = packet.decLayerL3Dev.IPv6.DecodeLayers(payload, &packet.Layers)
	default:
		err = packet.decLayerL3Dev.IPv4.DecodeLayers(payload, &packet.Layers)
	}

	if err != nil {
		return
	}

	for _, typ := range packet.Layers {
		summary = typ.String()
		switch typ {
		case layers.LayerTypeEthernet:
			ethernet = decodeEthernet(&packet.Ethernet)
		case layers.LayerTypeIPv4:
			ip, sourceIP, destinationIP = decodeIPv4(&packet.IPv4)
		case layers.LayerTypeIPv6:
			ip, sourceIP, destinationIP = decodeIPv6(&packet.IPv6)
		case layers.LayerTypeTCP:
			l4, sourcePort, destinationPort = decodeTCP(&packet.TCP)
			summary = "TCP Flags: " + getTCPFlags(packet.TCP)
		case layers.LayerTypeUDP:
			l4, sourcePort, destinationPort = decodeUDP(&packet.UDP)
		case layers.LayerTypeSCTP:
			l4, sourcePort, destinationPort = decodeSCTP(&packet.SCTP)
		case layers.LayerTypeICMPv4:
			l4 = decodeICMPv4(&packet.ICMPv4)
			summary = "ICMPv4 " + packet.ICMPv4.TypeCode.String()
		case layers.LayerTypeICMPv6:
			l4 = decodeICMPv6(&packet.ICMPv6)
			summary = "ICMPv6 " + packet.ICMPv6.TypeCode.String()
		}
	}

	switch {
	case isVXLAN:
		err = packet.decLayerOverlay.VXLAN.DecodeLayers(packet.UDP.Payload, &packet.overlay.Layers)
	case isGeneve:
		err = packet.decLayerOverlay.Geneve.DecodeLayers(packet.UDP.Payload, &packet.overlay.Layers)
	default:
		// Truncate layers to avoid accidental re-use.
		packet.overlay.Layers = packet.overlay.Layers[:0]
		return
	}

	if err != nil {
		err = fmt.Errorf("overlay: %w", err)
		return
	}

	// Return in case we have not decoded any overlay layer.
	if len(packet.overlay.Layers) == 0 {
		return
	}

	// Expect VXLAN/Geneve overlay as first overlay layer, if not we bail out.
	switch packet.overlay.Layers[0] {
	case layers.LayerTypeVXLAN:
		tunnel = &pb.Tunnel{Protocol: pb.Tunnel_VXLAN, IP: ip, L4: l4}
	case layers.LayerTypeGeneve:
		tunnel = &pb.Tunnel{Protocol: pb.Tunnel_GENEVE, IP: ip, L4: l4}
	default:
		return
	}

	// Reset return values. This ensures the resulting flow does not misrepresent
	// what is happening (e.g. same IP addresses for overlay and underlay).
	ethernet, ip, l4 = nil, nil, nil
	sourceIP, destinationIP = netip.Addr{}, netip.Addr{}
	sourcePort, destinationPort = 0, 0
	summary = ""

	// Parse the rest of the overlay layers as we would do for a non-encapsulated packet.
	// It is possible we're not parsing any layer here. This is because the overlay
	// decoders failed (e.g., not enough data). We would still return empty values
	// for the inner packet (ethernet, ip, l4, basically the re-init variables)
	// while returning the non-empty `tunnel` field.
	for _, typ := range packet.overlay.Layers[1:] {
		summary = typ.String()
		switch typ {
		case layers.LayerTypeEthernet:
			ethernet = decodeEthernet(&packet.overlay.Ethernet)
		case layers.LayerTypeIPv4:
			ip, sourceIP, destinationIP = decodeIPv4(&packet.overlay.IPv4)
		case layers.LayerTypeIPv6:
			ip, sourceIP, destinationIP = decodeIPv6(&packet.overlay.IPv6)
		case layers.LayerTypeTCP:
			l4, sourcePort, destinationPort = decodeTCP(&packet.overlay.TCP)
			summary = "TCP Flags: " + getTCPFlags(packet.overlay.TCP)
		case layers.LayerTypeUDP:
			l4, sourcePort, destinationPort = decodeUDP(&packet.overlay.UDP)
		case layers.LayerTypeSCTP:
			l4, sourcePort, destinationPort = decodeSCTP(&packet.overlay.SCTP)
		case layers.LayerTypeICMPv4:
			l4 = decodeICMPv4(&packet.overlay.ICMPv4)
			summary = "ICMPv4 " + packet.overlay.ICMPv4.TypeCode.String()
		case layers.LayerTypeICMPv6:
			l4 = decodeICMPv6(&packet.overlay.ICMPv6)
			summary = "ICMPv6 " + packet.overlay.ICMPv6.TypeCode.String()
		}
	}

	return
}

func decodeVerdict(dn *monitor.DropNotify, tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) pb.Verdict {
	switch {
	case dn != nil:
		return pb.Verdict_DROPPED
	case tn != nil:
		return pb.Verdict_FORWARDED
	case pvn != nil:
		if pvn.Verdict < 0 {
			return pb.Verdict_DROPPED
		}
		if pvn.Verdict > 0 {
			return pb.Verdict_REDIRECTED
		}
		if pvn.IsTrafficAudited() {
			return pb.Verdict_AUDIT
		}
		return pb.Verdict_FORWARDED
	}
	return pb.Verdict_VERDICT_UNKNOWN
}

func decodeDropReason(dn *monitor.DropNotify, pvn *monitor.PolicyVerdictNotify) uint32 {
	switch {
	case dn != nil:
		return uint32(dn.SubType)
	case pvn != nil && pvn.Verdict < 0:
		// if the flow was dropped, verdict equals the negative of the drop reason
		return uint32(-pvn.Verdict)
	}
	return 0
}

func decodeFileInfo(dn *monitor.DropNotify) *pb.FileInfo {
	switch {
	case dn != nil:
		return &pb.FileInfo{
			Name: monitorAPI.BPFFileName(dn.File),
			Line: uint32(dn.Line),
		}
	}
	return nil
}

func decodePolicyMatchType(pvn *monitor.PolicyVerdictNotify) uint32 {
	if pvn != nil {
		return uint32((pvn.Flags & monitor.PolicyVerdictNotifyFlagMatchType) >>
			monitor.PolicyVerdictNotifyFlagMatchTypeBitOffset)
	}
	return 0
}

func decodeEthernet(ethernet *layers.Ethernet) *pb.Ethernet {
	return &pb.Ethernet{
		Source:      ethernet.SrcMAC.String(),
		Destination: ethernet.DstMAC.String(),
	}
}

func decodeIPv4(ipv4 *layers.IPv4) (ip *pb.IP, src, dst netip.Addr) {
	// Ignore invalid IPs - getters will handle invalid values.
	// IPs can be empty for Ethernet-only packets.
	src, _ = netipx.FromStdIP(ipv4.SrcIP)
	dst, _ = netipx.FromStdIP(ipv4.DstIP)
	return &pb.IP{
		Source:      ipv4.SrcIP.String(),
		Destination: ipv4.DstIP.String(),
		IpVersion:   pb.IPVersion_IPv4,
	}, src, dst
}

func decodeIPv6(ipv6 *layers.IPv6) (ip *pb.IP, src, dst netip.Addr) {
	// Ignore invalid IPs - getters will handle invalid values.
	// IPs can be empty for Ethernet-only packets.
	src, _ = netipx.FromStdIP(ipv6.SrcIP)
	dst, _ = netipx.FromStdIP(ipv6.DstIP)
	return &pb.IP{
		Source:      ipv6.SrcIP.String(),
		Destination: ipv6.DstIP.String(),
		IpVersion:   pb.IPVersion_IPv6,
	}, src, dst
}

func decodeTCP(tcp *layers.TCP) (l4 *pb.Layer4, src, dst uint16) {
	return &pb.Layer4{
		Protocol: &pb.Layer4_TCP{
			TCP: &pb.TCP{
				SourcePort:      uint32(tcp.SrcPort),
				DestinationPort: uint32(tcp.DstPort),
				Flags: &pb.TCPFlags{
					FIN: tcp.FIN, SYN: tcp.SYN, RST: tcp.RST,
					PSH: tcp.PSH, ACK: tcp.ACK, URG: tcp.URG,
					ECE: tcp.ECE, CWR: tcp.CWR, NS: tcp.NS,
				},
			},
		},
	}, uint16(tcp.SrcPort), uint16(tcp.DstPort)
}

func decodeSCTP(sctp *layers.SCTP) (l4 *pb.Layer4, src, dst uint16) {
	return &pb.Layer4{
		Protocol: &pb.Layer4_SCTP{
			SCTP: &pb.SCTP{
				SourcePort:      uint32(sctp.SrcPort),
				DestinationPort: uint32(sctp.DstPort),
			},
		},
	}, uint16(sctp.SrcPort), uint16(sctp.DstPort)
}

func decodeUDP(udp *layers.UDP) (l4 *pb.Layer4, src, dst uint16) {
	return &pb.Layer4{
		Protocol: &pb.Layer4_UDP{
			UDP: &pb.UDP{
				SourcePort:      uint32(udp.SrcPort),
				DestinationPort: uint32(udp.DstPort),
			},
		},
	}, uint16(udp.SrcPort), uint16(udp.DstPort)
}

func decodeICMPv4(icmp *layers.ICMPv4) *pb.Layer4 {
	return &pb.Layer4{
		Protocol: &pb.Layer4_ICMPv4{ICMPv4: &pb.ICMPv4{
			Type: uint32(icmp.TypeCode.Type()),
			Code: uint32(icmp.TypeCode.Code()),
		}},
	}
}

func decodeICMPv6(icmp *layers.ICMPv6) *pb.Layer4 {
	return &pb.Layer4{
		Protocol: &pb.Layer4_ICMPv6{ICMPv6: &pb.ICMPv6{
			Type: uint32(icmp.TypeCode.Type()),
			Code: uint32(icmp.TypeCode.Code()),
		}},
	}
}

func decodeIsReply(tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) *wrapperspb.BoolValue {
	switch {
	case tn != nil && tn.TraceReasonIsKnown():
		if tn.TraceReasonIsEncap() || tn.TraceReasonIsDecap() {
			return nil
		}
		// Reason was specified by the datapath, just reuse it.
		return &wrapperspb.BoolValue{
			Value: tn.TraceReasonIsReply(),
		}
	case pvn != nil && pvn.Verdict >= 0:
		// Forwarded PolicyVerdictEvents are emitted for the first packet of
		// connection, therefore we statically assume that they are not reply
		// packets
		return &wrapperspb.BoolValue{Value: false}
	default:
		// For other events, such as drops, we simply do not know if they were
		// replies or not.
		return nil
	}
}

func decodeCiliumEventType(eventType, eventSubType uint8) *pb.CiliumEventType {
	return &pb.CiliumEventType{
		Type:    int32(eventType),
		SubType: int32(eventSubType),
	}
}

func decodeTraceReason(tn *monitor.TraceNotify) pb.TraceReason {
	if tn == nil {
		return pb.TraceReason_TRACE_REASON_UNKNOWN
	}
	// The Hubble protobuf enum values aren't 1:1 mapped with Cilium's datapath
	// because we want pb.TraceReason_TRACE_REASON_UNKNOWN = 0 while in
	// datapath monitor.TraceReasonUnknown = 5. The mapping works as follow:
	switch {
	// monitor.TraceReasonUnknown is mapped to pb.TraceReason_TRACE_REASON_UNKNOWN
	case tn.TraceReason() == monitor.TraceReasonUnknown:
		return pb.TraceReason_TRACE_REASON_UNKNOWN
	// values before monitor.TraceReasonUnknown are "offset by one", e.g.
	// TraceReasonCtEstablished = 1 → TraceReason_ESTABLISHED = 2 to make room
	// for the zero value.
	case tn.TraceReason() < monitor.TraceReasonUnknown:
		return pb.TraceReason(tn.TraceReason()) + 1
	// all values greater than monitor.TraceReasonUnknown are mapped 1:1 with
	// the datapath values.
	default:
		return pb.TraceReason(tn.TraceReason())
	}
}

func decodeSecurityIdentities(dn *monitor.DropNotify, tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) (
	sourceSecurityIdentiy, destinationSecurityIdentity uint32,
) {
	switch {
	case dn != nil:
		sourceSecurityIdentiy = uint32(dn.SrcLabel)
		destinationSecurityIdentity = uint32(dn.DstLabel)
	case tn != nil:
		sourceSecurityIdentiy = uint32(tn.SrcLabel)
		destinationSecurityIdentity = uint32(tn.DstLabel)
	case pvn != nil:
		if pvn.IsTrafficIngress() {
			sourceSecurityIdentiy = uint32(pvn.RemoteLabel)
		} else {
			destinationSecurityIdentity = uint32(pvn.RemoteLabel)
		}
	}

	return
}

func decodeTrafficDirection(srcEP uint32, dn *monitor.DropNotify, tn *monitor.TraceNotify, pvn *monitor.PolicyVerdictNotify) pb.TrafficDirection {
	if dn != nil && dn.Source != 0 {
		// If the local endpoint at which the drop occurred is the same as the
		// source of the dropped packet, we assume it was an egress flow. This
		// implies that we also assume that dropped packets are not dropped
		// reply packets of an ongoing connection.
		if dn.Source == uint16(srcEP) {
			return pb.TrafficDirection_EGRESS
		}
		return pb.TrafficDirection_INGRESS
	}
	if tn != nil && tn.Source != 0 {
		// For trace events, we assume that packets may be reply packets of an
		// ongoing connection. Therefore, we want to access the connection
		// tracking result from the `Reason` field to invert the direction for
		// reply packets. The datapath currently populates the `Reason` field
		// with CT information for some observation points.
		if tn.TraceReasonIsKnown() {
			// true if the traffic source is the local endpoint, i.e. egress
			isSourceEP := tn.Source == uint16(srcEP)
			// when OrigIP is set, then the packet was SNATed
			isSNATed := !tn.OriginalIP().IsUnspecified()
			// true if the packet is a reply, i.e. reverse direction
			isReply := tn.TraceReasonIsReply()

			switch {
			// Although technically the corresponding packet is ingressing the
			// stack (TraceReasonEncryptOverlay traces are TraceToStack), it is
			// ultimately originating from the local node and destinated to a
			// remote node, so egress make more sense to expose at a high
			// level.
			case tn.TraceReason() == monitor.TraceReasonEncryptOverlay:
				return pb.TrafficDirection_EGRESS
			// isSourceEP != isReply ==
			//  (isSourceEP && !isReply) || (!isSourceEP && isReply)
			case isSourceEP != isReply:
				return pb.TrafficDirection_EGRESS
			case isSNATed:
				return pb.TrafficDirection_EGRESS
			}
			return pb.TrafficDirection_INGRESS
		}
	}
	if pvn != nil {
		if pvn.IsTrafficIngress() {
			return pb.TrafficDirection_INGRESS
		}
		return pb.TrafficDirection_EGRESS
	}
	return pb.TrafficDirection_TRAFFIC_DIRECTION_UNKNOWN
}

func getTCPFlags(tcp layers.TCP) string {
	const (
		syn         = "SYN"
		ack         = "ACK"
		rst         = "RST"
		fin         = "FIN"
		psh         = "PSH"
		urg         = "URG"
		ece         = "ECE"
		cwr         = "CWR"
		ns          = "NS"
		maxTCPFlags = 9
		comma       = ", "
	)

	info := make([]string, 0, maxTCPFlags)

	if tcp.SYN {
		info = append(info, syn)
	}

	if tcp.ACK {
		info = append(info, ack)
	}

	if tcp.RST {
		info = append(info, rst)
	}

	if tcp.FIN {
		info = append(info, fin)
	}

	if tcp.PSH {
		info = append(info, psh)
	}

	if tcp.URG {
		info = append(info, urg)
	}

	if tcp.ECE {
		info = append(info, ece)
	}

	if tcp.CWR {
		info = append(info, cwr)
	}

	if tcp.NS {
		info = append(info, ns)
	}

	return strings.Join(info, comma)
}

func decodeDebugCapturePoint(dbg *monitor.DebugCapture) pb.DebugCapturePoint {
	if dbg == nil {
		return pb.DebugCapturePoint_DBG_CAPTURE_POINT_UNKNOWN
	}
	return pb.DebugCapturePoint(dbg.SubType)
}

func (p *Parser) decodeNetworkInterface(tn *monitor.TraceNotify, dbg *monitor.DebugCapture) *pb.NetworkInterface {
	ifIndex := uint32(0)
	if tn != nil {
		ifIndex = tn.Ifindex
	} else if dbg != nil {
		switch dbg.SubType {
		case monitor.DbgCaptureDelivery,
			monitor.DbgCaptureFromLb,
			monitor.DbgCaptureAfterV46,
			monitor.DbgCaptureAfterV64,
			monitor.DbgCaptureSnatPre,
			monitor.DbgCaptureSnatPost:
			ifIndex = dbg.Arg1
		}
	}

	if ifIndex == 0 {
		return nil
	}

	var name string
	if p.linkGetter != nil {
		// if the interface is not found, `name` will be an empty string and thus
		// omitted in the protobuf message
		name, _ = p.linkGetter.GetIfNameCached(int(ifIndex))
	}
	return &pb.NetworkInterface{
		Index: ifIndex,
		Name:  name,
	}
}

func decodeProxyPort(dbg *monitor.DebugCapture, tn *monitor.TraceNotify) uint32 {
	if tn != nil && tn.ObsPoint == monitorAPI.TraceToProxy {
		return uint32(tn.DstID)
	} else if dbg != nil {
		switch dbg.SubType {
		case monitor.DbgCaptureProxyPre,
			monitor.DbgCaptureProxyPost:
			return byteorder.NetworkToHost32(dbg.Arg1)
		}
	}

	return 0
}
