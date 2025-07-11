/* SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause) */
/* Copyright Authors of Cilium */

#pragma once

#include <bpf/config/node.h>

#include "lib/common.h"
#include "lib/ipv4.h"
#include "lib/ipv6.h"
#include "lib/l4.h"

typedef __u8 cls_flags_t;

/* Classification flags used to enrich trace/drop notifications events. */
enum {
	/* Packet uses IPv6. This flag is only needed/set in trace event:
	 * - carrying the orig_ip IPv6 info from send_trace_notify6, or
	 * - with L3 IPv6 packets, to instruct Hubble to use the right decoder.
	 */
	CLS_FLAG_IPV6	   = (1 << 0),
	/* Packet originates from a L3 device (no ethernet header). */
	CLS_FLAG_L3_DEV    = (1 << 1),
	/* Packet uses underlay VXLAN. */
	CLS_FLAG_VXLAN     = (1 << 2),
	/* Packet uses underlay Geneve. */
	CLS_FLAG_GENEVE    = (1 << 3),
};

/* Wrapper for specifying empty flags during the trace/drop event. */
#define CLS_FLAG_NONE ((cls_flags_t)0)

#ifdef HAVE_ENCAP
/* Return the correct overlay flag CLS_FLAG_{VXLAN,GENEVE} based on the current TUNNEL_PROTOCOL. */
#define CLS_FLAG_TUNNEL                               \
	(__builtin_constant_p(TUNNEL_PROTOCOL) ?              \
		((TUNNEL_PROTOCOL) == TUNNEL_PROTOCOL_VXLAN ? CLS_FLAG_VXLAN : \
		 (TUNNEL_PROTOCOL) == TUNNEL_PROTOCOL_GENEVE ? CLS_FLAG_GENEVE : \
		 (__throw_build_bug(), 0))                        \
	: (__throw_build_bug(), 0))

/**
 * can_observe_overlay_mark
 * @obs_point: trace observation point (TRACE_{FROM,TO}_*)
 *
 * Returns true whether the provided observation point can observe overlay traffic marked
 * with MARK_MAGIC_OVERLAY. This mark used in to-{netdev,wireguard}.
 */
static __always_inline bool
can_observe_overlay_mark(enum trace_point obs_point __maybe_unused)
{
# if __ctx_is == __ctx_skb
	if (is_defined(IS_BPF_HOST) && (obs_point == TRACE_TO_NETWORK ||
					obs_point == TRACE_POINT_UNKNOWN))
		return true;

	if (is_defined(IS_BPF_WIREGUARD) && (obs_point == TRACE_TO_CRYPTO ||
					     obs_point == TRACE_POINT_UNKNOWN))
		return true;
# endif /* __ctx_is == __ctx_skb */

	return false;
}

/**
 * can_observe_overlay_hdr
 * @obs_point: trace observation point (TRACE_{FROM,TO}_*)
 *
 * Returns true whether the provided observation point can observe overlay traffic via raw packet
 * parsing of L2/L3/L4 headers. Such packets are traced in from-{netdev,wireguard}, and in to-stack
 * events with ENABLE_IPSEC (VinE).
 */
static __always_inline bool
can_observe_overlay_hdr(enum trace_point obs_point)
{
	if (is_defined(IS_BPF_HOST) && (obs_point == TRACE_FROM_NETWORK ||
					obs_point == TRACE_POINT_UNKNOWN ||
					(is_defined(ENABLE_IPSEC) && obs_point == TRACE_TO_STACK)))
		return true;

	if (is_defined(IS_BPF_WIREGUARD) && (obs_point == TRACE_FROM_CRYPTO ||
					     obs_point == TRACE_POINT_UNKNOWN))
		return true;

	return false;
}
#endif /* HAVE_ENCAP */

/**
 * ctx_classify
 * @ctx: socket buffer
 * @proto: the layer 3 protocol (ETH_P_IP, ETH_P_IPV6).
 * @obs_point: the observation point (TRACE_{FROM,TO}_*).
 *
 * Compute classifiers (CLS_FLAG_*) for the given packet to be used during
 * trace/drop notification events. There exists two main computation methods:
 *
 * 1. inspecting ctx->mark for known magic values (ex. MARK_MAGIC_OVERLAY).
 * 3. inspecting L3/L4 headers for known traffic patterns (ex. UDP+OverlayPort).
 *
 * Both the two methods are optimized based on the observation point to preserve
 * performance and verifier complexity.
 */
static __always_inline cls_flags_t
ctx_classify(struct __ctx_buff *ctx, __be16 proto, enum trace_point obs_point __maybe_unused)
{
	cls_flags_t flags = CLS_FLAG_NONE;
	bool parse_overlay = false;
	void __maybe_unused *data;
	void __maybe_unused *data_end;
	struct ipv6hdr __maybe_unused *ip6;
	struct iphdr __maybe_unused *ip4;
	__be16 __maybe_unused dport;
	__u8 __maybe_unused l4_proto;
	int __maybe_unused l3_hdrlen;

	/*
	 * Retrieve protocol when not being provided.
	 * (ex. from drop notifications, or when previous calls to validate_ethertype failed)
	 */
	if (!proto)
		proto = ctx_get_protocol(ctx);

	/* Check whether the packet comes from a L3 device (no ethernet). */
	if (ETH_HLEN == 0)
		flags |= CLS_FLAG_L3_DEV;

	/* Check if IPv6 packet. */
	if (proto == bpf_htons(ETH_P_IPV6))
		flags |= CLS_FLAG_IPV6;

/* ctx->mark not available in XDP. */
#if __ctx_is == __ctx_skb
# ifdef HAVE_ENCAP
	if (can_observe_overlay_mark(obs_point) &&
	    (ctx->mark & MARK_MAGIC_HOST_MASK) == MARK_MAGIC_OVERLAY) {
		flags |= CLS_FLAG_TUNNEL;
		goto out;
	}
# endif /* HAVE_ENCAP */
#endif /* __ctx_skb */

#ifdef HAVE_ENCAP
	if (can_observe_overlay_hdr(obs_point))
		parse_overlay = true;
#endif /* HAVE_ENCAP */

	/*
	 * Skip subsequent logic that parses the packet L3/L4 headers
	 * when not needed. For new classifiers, let's use other variables `parse_*`.
	 */
	if (!parse_overlay)
		goto out;

	/*
	 * Inspect the L3 protocol, and retrieve l4_proto and l3_hdrlen.
	 * For IPv6, let's stop at the first header.
	 */
	switch (proto) {
# ifdef ENABLE_IPV6
	case bpf_htons(ETH_P_IPV6):
		if (!revalidate_data(ctx, &data, &data_end, &ip6))
			goto out;

		l4_proto = ip6->nexthdr;
		l3_hdrlen = sizeof(struct ipv6hdr);
		break;
# endif /* ENABLE_IPV6 */
# ifdef ENABLE_IPV4
	case bpf_htons(ETH_P_IP):
		if (!revalidate_data(ctx, &data, &data_end, &ip4))
			goto out;

		l4_proto = ip4->protocol;
		l3_hdrlen = ipv4_hdrlen(ip4);
		break;
# endif /* ENABLE_IPV4 */
	default:
		goto out;
	}

	/*
	 * Inspect the L4 protocol, looking for specific traffic patterns:
	 * - Overlay: UDP with destination port TUNNEL_PORT.
	 */
	switch (l4_proto) {
	case IPPROTO_UDP:
		if (l4_load_port(ctx, ETH_HLEN + l3_hdrlen + UDP_DPORT_OFF, &dport) < 0)
			goto out;
#ifdef HAVE_ENCAP
		if (parse_overlay && dport == bpf_htons(TUNNEL_PORT)) {
			flags |= CLS_FLAG_TUNNEL;
			goto out;
		}
#endif /* HAVE_ENCAP */
		break;
	}

out:
	return flags;
}

/**
 * compute_capture_len
 * @ctx: socket buffer
 * @monitor: the monitor value
 * @flags: the classifier flags (CLS_FLAG_*)
 * @obs_point: the trace observation point (TRACE_{FROM,TO}_*)
 *
 * Compute capture length for the trace/drop notification event.
 * Return at most `ctx_full_len` bytes.
 * With monitor=0, use the config value `trace_payload_len` for native packets, and
 * `trace_payload_len_overlay` for overlay packets with CLS_FLAG_{VXLAN,GENEVE} set. For overlay
 * packets, reuse the `obs_point` to save complexity.
 */
static __always_inline __u64
compute_capture_len(struct __ctx_buff *ctx, __u64 monitor,
		    cls_flags_t flags __maybe_unused,
		    enum trace_point obs_point __maybe_unused)
{
	__u32 cap_len_default = CONFIG(trace_payload_len);

#ifdef HAVE_ENCAP
	if ((can_observe_overlay_mark(obs_point) || can_observe_overlay_hdr(obs_point)) &&
	    flags & CLS_FLAG_TUNNEL)
		cap_len_default = CONFIG(trace_payload_len_overlay);
#endif

	if (monitor == 0 || monitor == CONFIG(trace_payload_len))
		monitor = cap_len_default;

	return min_t(__u64, monitor, ctx_full_len(ctx));
}
