package agent

import (
	"bufio"
	"math"
	stdnet "net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/mem"
	gnet "github.com/shirou/gopsutil/v3/net"
	"github.com/shirou/gopsutil/v3/process"

	"mote/internal/shared"
)

// 单条上报里 Disks 数组的硬上限,防御性保护(容器化宿主可能挂载几十上百个 mount)
const maxDisksPerReport = 32

// TCP/UDP 连接数采集的最小间隔。每 2 秒一次的全量 connections 扫描开销不小,
// 改成低频采样并缓存。读 /proc/net/sockstat 几乎零开销,优先走它。
const connCountInterval = 10 * time.Second

// CollectHello 采集一次性的主机信息(连接时发送)
func CollectHello(cfg *Config) *shared.HelloPayload {
	machineID := readMachineID()
	h := &shared.HelloPayload{
		ProtocolVersion:  shared.ProtocolVersion,
		AgentVersion:     shared.Version,
		Token:            cfg.Token,
		AutoDiscoveryKey: cfg.AutoDiscovery,
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		MachineID:        machineID,
		PrimaryMAC:       readPrimaryMAC(),
		MachineIDSuspect: isSuspectMachineID(machineID),
	}
	if info, err := host.Info(); err == nil {
		h.Hostname = info.Hostname
		h.Platform = info.Platform
		h.PlatformVer = info.PlatformVersion
		h.KernelVersion = info.KernelVersion
		h.BootTime = int64(info.BootTime)
	}
	if cpus, err := cpu.Info(); err == nil && len(cpus) > 0 {
		h.CPUModel = cpus[0].ModelName
	}
	if cnt, err := cpu.Counts(true); err == nil {
		h.CPUCores = cnt
	}
	if v, err := mem.VirtualMemory(); err == nil {
		h.MemTotal = v.Total
	}
	if parts, err := disk.Partitions(false); err == nil {
		// 按底层设备去重,避免 bind mount / subvolume / loop 重复计入
		seen := make(map[string]bool)
		var total uint64
		for _, p := range parts {
			if !isRealFS(p.Fstype) {
				continue
			}
			if p.Device != "" {
				if seen[p.Device] {
					continue
				}
				seen[p.Device] = true
			}
			if u, err := disk.Usage(p.Mountpoint); err == nil {
				total += u.Total
			}
		}
		h.DiskTotal = total
	}
	return h
}

// readMachineID 读取 /etc/machine-id(或 /var/lib/dbus/machine-id),非 Linux 返回空。
func readMachineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if data, err := os.ReadFile(p); err == nil {
			id := strings.TrimSpace(string(data))
			if id != "" {
				return id
			}
		}
	}
	return ""
}

// isSuspectMachineID 检测全零/全f/已知 Docker 默认值等不可靠的 machine-id。
func isSuspectMachineID(id string) bool {
	if id == "" {
		return true
	}
	// 全零或全 f
	first := id[0]
	allSame := true
	for i := 1; i < len(id); i++ {
		if id[i] != first {
			allSame = false
			break
		}
	}
	if allSame {
		return true
	}
	// 已知 Docker / 克隆 VM 常见默认值
	suspect := map[string]bool{
		"abcdef0123456789abcdef0123456789": true,
		"00000000000000000000000000000000": true,
		"ffffffffffffffffffffffffffffffff": true,
	}
	return suspect[strings.ToLower(strings.ReplaceAll(id, "-", ""))]
}

// readPrimaryMAC 取第一块"看起来是物理网卡"的 MAC 地址。仅作为机器指纹辅助。
// 用标准库 net.Interfaces,避免再多走一次 gopsutil 的封装。
func readPrimaryMAC() string {
	ifs, err := stdnet.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifs {
		if len(ifc.HardwareAddr) == 0 {
			continue
		}
		// 跳过 loopback 与 point-to-point
		if ifc.Flags&stdnet.FlagLoopback != 0 {
			continue
		}
		name := ifc.Name
		// 过滤虚拟接口(与 NIC 默认排除规则保持一致)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "tun") || strings.HasPrefix(name, "tap") ||
			strings.HasPrefix(name, "wg") || strings.HasPrefix(name, "cni") || strings.HasPrefix(name, "flannel") ||
			strings.HasPrefix(name, "cali") || strings.HasPrefix(name, "kube") ||
			strings.HasPrefix(name, "virbr") || strings.HasPrefix(name, "vmnet") {
			continue
		}
		return ifc.HardwareAddr.String()
	}
	return ""
}

// Collector 维护采集状态(主要是网络字节数 last 值,以及 TCP/UDP 连接数缓存)
type Collector struct {
	cfg         *Config
	probe       *probeTracker
	nicIncRegex *regexp.Regexp
	nicExcRegex *regexp.Regexp

	lastNetIn    uint64
	lastNetOut   uint64
	lastNetTime  time.Time
	initialized  bool

	// TCP/UDP 连接数缓存(低频采样)
	lastConnTime time.Time
	lastTCPConn  int
	lastUDPConn  int
}

// NewCollectorSimple 创建无拨测的 Collector，供主控自监控（server 包）调用。
func NewCollectorSimple(cfg *Config) *Collector {
	return NewCollector(cfg, nil)
}

func NewCollector(cfg *Config, probe *probeTracker) *Collector {
	c := &Collector{cfg: cfg, probe: probe}
	if cfg.NICInclude != "" {
		c.nicIncRegex = regexp.MustCompile(cfg.NICInclude)
	}
	if cfg.NICExclude != "" {
		c.nicExcRegex = regexp.MustCompile(cfg.NICExclude)
	}
	return c
}

// Collect 采集一次指标
func (c *Collector) Collect() *shared.MetricPayload {
	now := time.Now()
	m := &shared.MetricPayload{
		Timestamp: now.Unix(),
	}

	// CPU:只采一次 per-core,然后自己求平均得到整体值。
	// 不同时调用 cpu.Percent(0, true) 和 cpu.Percent(0, false) ——
	// gopsutil 内部 percpu=true/false 共享同一个 last-value 缓存,
	// 第二次调用拿不到正确的差值基准,会导致整体使用率失真。
	if pcts, err := cpu.Percent(0, true); err == nil && len(pcts) > 0 {
		m.CPUPerCore = make([]float64, len(pcts))
		var sum float64
		for i, p := range pcts {
			m.CPUPerCore[i] = round2(p)
			sum += p
		}
		m.CPUUsage = round2(sum / float64(len(pcts)))
	}

	// 负载
	if l, err := load.Avg(); err == nil {
		m.Load1 = round2(l.Load1)
		m.Load5 = round2(l.Load5)
		m.Load15 = round2(l.Load15)
	}

	// 内存
	if v, err := mem.VirtualMemory(); err == nil {
		m.MemUsed = v.Used
		m.MemTotal = v.Total
	}
	if s, err := mem.SwapMemory(); err == nil {
		m.SwapUsed = s.Used
		m.SwapTotal = s.Total
	}

	// 磁盘
	if parts, err := disk.Partitions(false); err == nil {
		seen := make(map[string]bool)
		for _, p := range parts {
			if !isRealFS(p.Fstype) {
				continue
			}
			// 按 device 去重(bind mount 同一设备多个挂载点)
			if p.Device != "" {
				if seen[p.Device] {
					continue
				}
				seen[p.Device] = true
			}
			if u, err := disk.Usage(p.Mountpoint); err == nil && u.Total > 0 {
				if len(m.Disks) >= maxDisksPerReport {
					m.DisksTruncated = true
					break
				}
				m.Disks = append(m.Disks, shared.DiskInfo{
					Mountpoint: p.Mountpoint,
					Used:       u.Used,
					Total:      u.Total,
				})
			}
		}
	}

	// 网络:累加过滤后的网卡
	var inBytes, outBytes uint64
	if stats, err := gnet.IOCounters(true); err == nil {
		for _, s := range stats {
			if !c.shouldIncludeNIC(s.Name) {
				continue
			}
			inBytes += s.BytesRecv
			outBytes += s.BytesSent
		}
	}

	if c.initialized {
		dt := now.Sub(c.lastNetTime).Seconds()
		if dt <= 0 {
			dt = 1
		}
		dIn := safeDelta(inBytes, c.lastNetIn)
		dOut := safeDelta(outBytes, c.lastNetOut)
		m.NetInDelta = dIn
		m.NetOutDelta = dOut
		m.NetInSpeed = uint64(float64(dIn) / dt)
		m.NetOutSpeed = uint64(float64(dOut) / dt)
	}
	c.lastNetIn = inBytes
	c.lastNetOut = outBytes
	c.lastNetTime = now
	c.initialized = true

	// 进程数(轻量,每次都采)
	if pids, err := process.Pids(); err == nil {
		m.ProcessCount = len(pids)
	}

	// TCP/UDP 连接数:低频采样,优先走 /proc/net/sockstat(几乎零开销)。
	if now.Sub(c.lastConnTime) >= connCountInterval || c.lastConnTime.IsZero() {
		tcp, udp, ok := readSockstatCounts()
		if !ok {
			// 非 Linux 或读不到,退回 gopsutil(代价较高,所以本来就是低频)
			if conns, err := gnet.Connections("tcp"); err == nil {
				tcp = len(conns)
			}
			if conns, err := gnet.Connections("udp"); err == nil {
				udp = len(conns)
			}
		}
		c.lastTCPConn = tcp
		c.lastUDPConn = udp
		c.lastConnTime = now
	}
	m.TCPConn = c.lastTCPConn
	m.UDPConn = c.lastUDPConn

	// 运行时长
	if bt, err := host.BootTime(); err == nil {
		m.Uptime = uint64(now.Unix()) - bt
	}
	if c.probe != nil {
		m.LatencyMS, m.LossPct = c.probe.snapshotAndReset()
	}

	return m
}

// readSockstatCounts 读取 /proc/net/sockstat (IPv4) 和 sockstat6 (IPv6)。
// 仅适用于 Linux;非 Linux 返回 ok=false。
//
// /proc/net/sockstat 格式样例:
//   sockets: used 234
//   TCP: inuse 12 orphan 0 tw 4 alloc 16 mem 1
//   UDP: inuse 6 mem 2
//   ...
func readSockstatCounts() (tcp, udp int, ok bool) {
	tcp4, udp4, ok4 := parseSockstat("/proc/net/sockstat")
	tcp6, udp6, _ := parseSockstat("/proc/net/sockstat6")
	if !ok4 {
		return 0, 0, false
	}
	return tcp4 + tcp6, udp4 + udp6, true
}

func parseSockstat(path string) (tcp, udp int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// 行格式: "TCP: inuse 12 ..." 或 "TCP6: inuse 0 ..."
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		head := fields[0]
		if (head == "TCP:" || head == "TCP6:") && fields[1] == "inuse" {
			n := atoiSafe(fields[2])
			tcp += n
			ok = true
		} else if (head == "UDP:" || head == "UDP6:") && fields[1] == "inuse" {
			n := atoiSafe(fields[2])
			udp += n
			ok = true
		}
	}
	return tcp, udp, ok
}

func atoiSafe(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func (c *Collector) shouldIncludeNIC(name string) bool {
	// 黑名单优先
	if c.nicExcRegex != nil && c.nicExcRegex.MatchString(name) {
		return false
	}
	// 白名单(如果配了)
	if c.nicIncRegex != nil {
		return c.nicIncRegex.MatchString(name)
	}
	return true
}

func isRealFS(fstype string) bool {
	fstype = strings.ToLower(fstype)
	bad := []string{"tmpfs", "devtmpfs", "proc", "sysfs", "cgroup", "cgroup2",
		"overlay", "squashfs", "fusectl", "debugfs", "tracefs", "configfs",
		"securityfs", "pstore", "bpf", "ramfs", "autofs", "mqueue", "hugetlbfs",
		"nsfs", "rpc_pipefs", "binfmt_misc", "fuse.gvfsd-fuse", "fuse.portal"}
	for _, b := range bad {
		if fstype == b {
			return false
		}
	}
	return fstype != ""
}

// safeDelta 计算增量,处理 counter 回绕(网卡重置)
func safeDelta(cur, last uint64) uint64 {
	if cur < last {
		return 0 // 回绕了,跳过这次
	}
	d := cur - last
	// 防异常:超过 10GB/秒 视为异常跳过
	if d > 10*1024*1024*1024 {
		return 0
	}
	return d
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
