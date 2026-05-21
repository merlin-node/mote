package agent

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"time"
)

// probeTCP 通过 TCP dial 测量延迟，适用于任意带端口的服务
func probeTCP(ip string, port int, timeout time.Duration) (latencyMS float64, success bool) {
	addr := fmt.Sprintf("%s:%d", ip, port)
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, false
	}
	conn.Close()
	return float64(time.Since(start).Milliseconds()), true
}

// probeICMP 通过原始 ICMP socket 发送 Echo Request 测量延迟（需要 root/CAP_NET_RAW）
func probeICMP(ip string, timeout time.Duration) (latencyMS float64, success bool) {
	conn, err := net.DialTimeout("ip4:icmp", ip, timeout)
	if err != nil {
		return 0, false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))

	id := uint16(os.Getpid() & 0xffff)
	msg := buildICMPEcho(id, 1)

	start := time.Now()
	if _, err := conn.Write(msg); err != nil {
		return 0, false
	}

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return 0, false
	}
	// 跳过 IP 头（通常 20 字节）取 ICMP 头
	if n < 28 {
		return 0, false
	}
	icmp := buf[20:n]
	// ICMP Echo Reply type = 0
	if icmp[0] != 0 {
		return 0, false
	}
	// 验证 ID
	replyID := binary.BigEndian.Uint16(icmp[4:6])
	if replyID != id {
		return 0, false
	}
	return float64(time.Since(start).Milliseconds()), true
}

func buildICMPEcho(id, seq uint16) []byte {
	msg := make([]byte, 8)
	msg[0] = 8 // Type: Echo Request
	msg[1] = 0 // Code: 0
	// Checksum placeholder
	binary.BigEndian.PutUint16(msg[4:], id)
	binary.BigEndian.PutUint16(msg[6:], seq)
	cs := icmpChecksum(msg)
	binary.BigEndian.PutUint16(msg[2:], cs)
	return msg
}

func icmpChecksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
