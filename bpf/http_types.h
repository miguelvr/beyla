#ifndef HTTP_TYPES_H
#define HTTP_TYPES_H

#include "common.h"
#include "bpf_helpers.h"
#include "http_defs.h"

#define F_HTTP_SRV  0x4
#define F_HTTP_CLNT 0x8

#define BUFFER_SIZE 192

// Struct to keep information on the connections in flight 
// s = source, d = destination
// h = high word, l = low word
// used as hashmap key, must be 4 byte aligned?
typedef struct http_connection_info {
    u8 s_addr[IP_V6_ADDR_LEN];
    u8 d_addr[IP_V6_ADDR_LEN];
    u16 s_port;
    u16 d_port;
    u32 flags;
} http_connection_info_t;

// Here we keep the information that is sent on the ring buffer
typedef struct http_info {
    http_connection_info_t info;
    u64 req_start_monotime_ns;
    u64 start_monotime_ns;
    u64 end_monotime_ns;
    u8  request_method;
    u16 response_status_code;
    u32 pid; // we need this for system wide tracking so we can find the service name
    unsigned char buf[BUFFER_SIZE];
} http_info_t;

// Force emitting struct http_request_trace into the ELF for automatic creation of Golang struct
const http_info_t *unused __attribute__((unused));

#if defined(__TARGET_ARCH_arm64)
// Copied from Linux include/uapi/asm/ptrace.h to make ARM64 happy
struct user_pt_regs {
	u64		regs[31];
	u64		sp;
	u64		pc;
	u64		pstate;
};
#endif

#ifdef BPF_DEBUG
static __always_inline void dbg_print_http_connection_info(http_connection_info_t *info) {
    bpf_printk("[http %s] s_l = %llx, s_h = %llx, d_l = %llx, d_h = %llx, s_port=%d, "
               "d_port=%d, flags=%llx",
               (info->flags & F_HTTP_SRV) ? "server" : "client",
               *(u64 *)(&info->s_addr),
               *(u64 *)(&info->s_addr[8]),
               *(u64 *)(&info->d_addr),
               *(u64 *)(&info->d_addr[8]),
               info->s_port,
               info->d_port,
               info->flags);
}
#else
static __always_inline void dbg_print_http_connection_info(http_connection_info_t *info) {
}
#endif

static __always_inline bool likely_ephemeral_port(u16 port) {
    return port >= EPHEMERAL_PORT_MIN;
}

#define __SWAP(T, x, y)                                                                            \
    {                                                                                              \
        T TMP = x;                                                                                 \
        x = y;                                                                                     \
        y = TMP;                                                                                   \
    }

// Since we track both send and receive connections, we need to sort the source and destination
// pairs in a standardized way, we choose the server way of sorting, such that the ephemeral port
// on the client is first.
static __always_inline void sort_connection_info(http_connection_info_t *info) {
    if (likely_ephemeral_port(info->s_port) && !likely_ephemeral_port(info->d_port)) {
        return;
    }

    if ((likely_ephemeral_port(info->d_port) && !likely_ephemeral_port(info->s_port)) ||
        (info->d_port > info->s_port)) {
        // Only sort if they are explicitly reversed, otherwise always sort source to be the larger
        // of the two ports
        __SWAP(u16, info->s_port, info->d_port);
        u8 tmp_addr[IP_V6_ADDR_LEN];
        __builtin_memcpy(tmp_addr, info->s_addr, sizeof(tmp_addr));
        __builtin_memcpy(info->s_addr, info->d_addr, sizeof(info->s_addr));
        __builtin_memcpy(info->d_addr, tmp_addr, sizeof(info->d_addr));
    }
}

#endif