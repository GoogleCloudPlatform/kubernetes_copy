// Copyright 2018 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package libcontainer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs2"
	"k8s.io/klog/v2"

	"github.com/google/cadvisor/container"
	"github.com/google/cadvisor/container/common"
	info "github.com/google/cadvisor/info/v1"
)

var (
	referencedResetInterval = flag.Uint64("referenced_reset_interval", 0,
		"Reset interval for referenced bytes (container_referenced_bytes metric), number of measurement cycles after which referenced bytes are cleared, if set to 0 referenced bytes are never cleared (default: 0)")

	smapsFilePathPattern     = "/proc/%d/smaps"
	clearRefsFilePathPattern = "/proc/%d/clear_refs"

	referencedRegexp = regexp.MustCompile(`Referenced:\s*([0-9]+)\s*kB`)
)

type Handler struct {
	cgroupManager   cgroups.Manager
	rootFs          string
	pid             int
	includedMetrics container.MetricSet
	// pidMetricsCache holds CPU scheduler stats for existing processes (map key is PID) between calls to schedulerStatsFromProcs.
	pidMetricsCache map[int]*info.CpuSchedstat
	// pidMetricsSaved holds accumulated CPU scheduler stats for processes that no longer exist.
	pidMetricsSaved info.CpuSchedstat
	cycles          uint64
}

func NewHandler(cgroupManager cgroups.Manager, rootFs string, pid int, includedMetrics container.MetricSet) *Handler {
	return &Handler{
		cgroupManager:   cgroupManager,
		rootFs:          rootFs,
		pid:             pid,
		includedMetrics: includedMetrics,
		pidMetricsCache: make(map[int]*info.CpuSchedstat),
	}
}

// Get cgroup and networking stats of the specified container
func (h *Handler) GetStats() (*info.ContainerStats, error) {
	ignoreStatsError := false
	if cgroups.IsCgroup2UnifiedMode() {
		// On cgroup v2 the root cgroup stats have been introduced in recent kernel versions,
		// so not all kernel versions have all the data. This means that stat fetching can fail
		// due to lacking cgroup stat files, but that some data is provided.
		if h.cgroupManager.Path("") == fs2.UnifiedMountpoint {
			ignoreStatsError = true
		}
	}

	cgroupStats, err := h.cgroupManager.GetStats()
	if err != nil {
		if !ignoreStatsError {
			return nil, err
		}
		klog.V(4).Infof("Ignoring errors when gathering stats for root cgroup since some controllers don't have stats on the root cgroup: %v", err)
	}
	stats := newContainerStats(cgroupStats, h.includedMetrics)

	if h.includedMetrics.Has(container.ProcessSchedulerMetrics) {
		stats.Cpu.Schedstat, err = h.schedulerStatsFromProcs()
		if err != nil {
			klog.V(4).Infof("Unable to get Process Scheduler Stats: %v", err)
		}
	}

	if h.includedMetrics.Has(container.ReferencedMemoryMetrics) {
		h.cycles++
		pids, err := h.cgroupManager.GetPids()
		if err != nil {
			klog.V(4).Infof("Could not get PIDs for container %d: %v", h.pid, err)
		} else {
			stats.ReferencedMemory, err = referencedBytesStat(pids, h.cycles, *referencedResetInterval)
			if err != nil {
				klog.V(4).Infof("Unable to get referenced bytes: %v", err)
			}
		}
	}

	// If we know the pid then get network stats from /proc/<pid>/net/dev
	if h.pid > 0 {
		if h.includedMetrics.Has(container.NetworkUsageMetrics) {
			netStats, err := networkStatsFromProc(h.rootFs, h.pid)
			if err != nil {
				klog.V(4).Infof("Unable to get network stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.Interfaces = append(stats.Network.Interfaces, netStats...)
			}
		}
		if h.includedMetrics.Has(container.NetworkTcpUsageMetrics) {
			t, err := tcpStatsFromProc(h.rootFs, h.pid, "net/tcp")
			if err != nil {
				klog.V(4).Infof("Unable to get tcp stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.Tcp = t
			}

			t6, err := tcpStatsFromProc(h.rootFs, h.pid, "net/tcp6")
			if err != nil {
				klog.V(4).Infof("Unable to get tcp6 stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.Tcp6 = t6
			}

		}
		if h.includedMetrics.Has(container.NetworkAdvancedTcpUsageMetrics) {
			ta, err := advancedTCPStatsFromProc(h.rootFs, h.pid, "net/netstat", "net/snmp")
			if err != nil {
				klog.V(4).Infof("Unable to get advanced tcp stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.TcpAdvanced = ta
			}
		}
		if h.includedMetrics.Has(container.NetworkUdpUsageMetrics) {
			u, err := udpStatsFromProc(h.rootFs, h.pid, "net/udp")
			if err != nil {
				klog.V(4).Infof("Unable to get udp stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.Udp = u
			}

			u6, err := udpStatsFromProc(h.rootFs, h.pid, "net/udp6")
			if err != nil {
				klog.V(4).Infof("Unable to get udp6 stats from pid %d: %v", h.pid, err)
			} else {
				stats.Network.Udp6 = u6
			}
		}
	}
	// some process metrics are per container ( number of processes, number of
	// file descriptors etc.) and not required a proper container's
	// root PID (systemd services don't have the root PID atm)
	if h.includedMetrics.Has(container.ProcessMetrics) {
		path, ok := common.GetControllerPath(h.cgroupManager.GetPaths(), "cpu", cgroups.IsCgroup2UnifiedMode())
		if !ok {
			klog.V(4).Infof("Could not find cgroups CPU for container %d", h.pid)
		} else {
			stats.Processes, err = processStatsFromProcs(h.rootFs, path, h.pid)
			if err != nil {
				klog.V(4).Infof("Unable to get Process Stats: %v", err)
			}
		}

		// if include processes metrics, just set threads metrics if exist, and has no relationship with cpu path
		setThreadsStats(cgroupStats, stats)
	}

	// For backwards compatibility.
	if len(stats.Network.Interfaces) > 0 {
		stats.Network.InterfaceStats = stats.Network.Interfaces[0]
	}

	return stats, nil
}

func parseUlimit(value string) (int64, error) {
	num, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		if strings.EqualFold(value, "unlimited") {
			// -1 implies unlimited except for priority and nice; man limits.conf
			num = -1
		} else {
			// Value is not a number or "unlimited"; return an error
			return 0, fmt.Errorf("unable to parse limit: %s", value)
		}
	}
	return num, nil
}

func processLimitsFile(fileData string) []info.UlimitSpec {
	const maxOpenFilesLinePrefix = "Max open files"

	limits := strings.Split(fileData, "\n")
	ulimits := make([]info.UlimitSpec, 0, len(limits))
	for _, lim := range limits {
		// Skip any headers/footers
		if strings.HasPrefix(lim, "Max open files") {
			// Remove line prefix
			ulimit, err := processMaxOpenFileLimitLine(
				"max_open_files",
				lim[len(maxOpenFilesLinePrefix):],
			)
			if err == nil {
				ulimits = append(ulimits, ulimit)
			}
		}
	}
	return ulimits
}

// Any caller of processMaxOpenFileLimitLine must ensure that the name prefix is already removed from the limit line.
// with the "Max open files" prefix.
func processMaxOpenFileLimitLine(name, line string) (info.UlimitSpec, error) {
	// Remove any leading whitespace
	line = strings.TrimSpace(line)
	// Split on whitespace
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return info.UlimitSpec{}, fmt.Errorf("unable to parse max open files line: %s", line)
	}
	// The first field is the soft limit, the second is the hard limit
	soft, err := parseUlimit(fields[0])
	if err != nil {
		return info.UlimitSpec{}, err
	}
	hard, err := parseUlimit(fields[1])
	if err != nil {
		return info.UlimitSpec{}, err
	}
	return info.UlimitSpec{
		Name:      name,
		SoftLimit: soft,
		HardLimit: hard,
	}, nil
}

func processRootProcUlimits(rootFs string, rootPid int) []info.UlimitSpec {
	filePath := path.Join(rootFs, "/proc", strconv.Itoa(rootPid), "limits")
	out, err := os.ReadFile(filePath)
	if err != nil {
		klog.V(4).Infof("error while listing directory %q to read ulimits: %v", filePath, err)
		return []info.UlimitSpec{}
	}
	return processLimitsFile(string(out))
}

func processStatsFromProcs(rootFs string, cgroupPath string, rootPid int) (info.ProcessStats, error) {
	var fdCount, socketCount uint64
	filePath := path.Join(cgroupPath, "cgroup.procs")
	out, err := os.ReadFile(filePath)
	if err != nil {
		return info.ProcessStats{}, fmt.Errorf("couldn't open cpu cgroup procs file %v : %v", filePath, err)
	}

	pids := strings.Split(string(out), "\n")

	// EOL is also treated as a new line while reading "cgroup.procs" file with os.ReadFile.
	// The last value is an empty string "". Ex: pids = ["22", "1223", ""]
	// Trim the last value
	if len(pids) != 0 && pids[len(pids)-1] == "" {
		pids = pids[:len(pids)-1]
	}

	for _, pid := range pids {
		dirPath := path.Join(rootFs, "/proc", pid, "fd")
		fds, err := os.ReadDir(dirPath)
		if err != nil {
			klog.V(4).Infof("error while listing directory %q to measure fd count: %v", dirPath, err)
			continue
		}
		fdCount += uint64(len(fds))
		for _, fd := range fds {
			fdPath := path.Join(dirPath, fd.Name())
			linkName, err := os.Readlink(fdPath)
			if err != nil {
				klog.V(4).Infof("error while reading %q link: %v", fdPath, err)
				continue
			}
			if strings.HasPrefix(linkName, "socket") {
				socketCount++
			}
		}
	}

	processStats := info.ProcessStats{
		ProcessCount: uint64(len(pids)),
		FdCount:      fdCount,
		SocketCount:  socketCount,
	}

	if rootPid > 0 {
		processStats.Ulimits = processRootProcUlimits(rootFs, rootPid)
	}

	return processStats, nil
}

func (h *Handler) schedulerStatsFromProcs() (info.CpuSchedstat, error) {
	pids, err := h.cgroupManager.GetAllPids()
	if err != nil {
		return info.CpuSchedstat{}, fmt.Errorf("Could not get PIDs for container %d: %w", h.pid, err)
	}
	alivePids := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		f, err := os.Open(path.Join(h.rootFs, "proc", strconv.Itoa(pid), "schedstat"))
		if err != nil {
			return info.CpuSchedstat{}, fmt.Errorf("couldn't open scheduler statistics for process %d: %v", pid, err)
		}
		defer f.Close()
		contents, err := io.ReadAll(f)
		if err != nil {
			return info.CpuSchedstat{}, fmt.Errorf("couldn't read scheduler statistics for process %d: %v", pid, err)
		}
		alivePids[pid] = struct{}{}
		rawMetrics := bytes.Split(bytes.TrimRight(contents, "\n"), []byte(" "))
		if len(rawMetrics) != 3 {
			return info.CpuSchedstat{}, fmt.Errorf("unexpected number of metrics in schedstat file for process %d", pid)
		}
		cacheEntry, ok := h.pidMetricsCache[pid]
		if !ok {
			cacheEntry = &info.CpuSchedstat{}
			h.pidMetricsCache[pid] = cacheEntry
		}
		for i, rawMetric := range rawMetrics {
			metric, err := strconv.ParseUint(string(rawMetric), 10, 64)
			if err != nil {
				return info.CpuSchedstat{}, fmt.Errorf("parsing error while reading scheduler statistics for process: %d: %v", pid, err)
			}
			switch i {
			case 0:
				cacheEntry.RunTime = metric
			case 1:
				cacheEntry.RunqueueTime = metric
			case 2:
				cacheEntry.RunPeriods = metric
			}
		}
	}
	schedstats := h.pidMetricsSaved // copy
	for p, v := range h.pidMetricsCache {
		schedstats.RunPeriods += v.RunPeriods
		schedstats.RunqueueTime += v.RunqueueTime
		schedstats.RunTime += v.RunTime
		if _, alive := alivePids[p]; !alive {
			// PID p is gone: accumulate its stats ...
			h.pidMetricsSaved.RunPeriods += v.RunPeriods
			h.pidMetricsSaved.RunqueueTime += v.RunqueueTime
			h.pidMetricsSaved.RunTime += v.RunTime
			// ... and remove its cache entry, to prevent
			// pidMetricsCache from growing.
			delete(h.pidMetricsCache, p)
		}
	}
	return schedstats, nil
}

// referencedBytesStat gets and clears referenced bytes
// see: https://github.com/brendangregg/wss#wsspl-referenced-page-flag
func referencedBytesStat(pids []int, cycles uint64, resetInterval uint64) (uint64, error) {
	referencedKBytes, err := getReferencedKBytes(pids)
	if err != nil {
		return uint64(0), err
	}

	err = clearReferencedBytes(pids, cycles, resetInterval)
	if err != nil {
		return uint64(0), err
	}
	return referencedKBytes * 1024, nil
}

func getReferencedKBytes(pids []int) (uint64, error) {
	referencedKBytes := uint64(0)
	readSmapsContent := false
	foundMatch := false
	for _, pid := range pids {
		smapsFilePath := fmt.Sprintf(smapsFilePathPattern, pid)
		smapsContent, err := os.ReadFile(smapsFilePath)
		if err != nil {
			klog.V(5).Infof("Cannot read %s file, err: %s", smapsFilePath, err)
			if os.IsNotExist(err) {
				continue // smaps file does not exists for all PIDs
			}
			return 0, err
		}
		readSmapsContent = true

		allMatches := referencedRegexp.FindAllSubmatch(smapsContent, -1)
		if len(allMatches) == 0 {
			klog.V(5).Infof("Not found any information about referenced bytes in %s file", smapsFilePath)
			continue // referenced bytes may not exist in smaps file
		}

		for _, matches := range allMatches {
			if len(matches) != 2 {
				return 0, fmt.Errorf("failed to match regexp in output: %s", string(smapsContent))
			}
			foundMatch = true
			referenced, err := strconv.ParseUint(string(matches[1]), 10, 64)
			if err != nil {
				return 0, err
			}
			referencedKBytes += referenced
		}
	}

	if len(pids) != 0 {
		if !readSmapsContent {
			klog.Warningf("Cannot read smaps files for any PID from %s", "CONTAINER")
		} else if !foundMatch {
			klog.Warningf("Not found any information about referenced bytes in smaps files for any PID from %s", "CONTAINER")
		}
	}
	return referencedKBytes, nil
}

func clearReferencedBytes(pids []int, cycles uint64, resetInterval uint64) error {
	if resetInterval == 0 {
		return nil
	}

	if cycles%resetInterval == 0 {
		for _, pid := range pids {
			clearRefsFilePath := fmt.Sprintf(clearRefsFilePathPattern, pid)
			clerRefsFile, err := os.OpenFile(clearRefsFilePath, os.O_WRONLY, 0o644)
			if err != nil {
				// clear_refs file may not exist for all PIDs
				continue
			}
			_, err = clerRefsFile.WriteString("1\n")
			if err != nil {
				return err
			}
			err = clerRefsFile.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func networkStatsFromProc(rootFs string, pid int) ([]info.InterfaceStats, error) {
	netStatsFile := path.Join(rootFs, "proc", strconv.Itoa(pid), "/net/dev")

	ifaceStats, err := scanInterfaceStats(netStatsFile)
	if err != nil {
		return []info.InterfaceStats{}, fmt.Errorf("couldn't read network stats: %v", err)
	}

	return ifaceStats, nil
}

var ignoredDevicePrefixes = []string{"lo", "veth", "docker", "nerdctl"}

func isIgnoredDevice(ifName string) bool {
	for _, prefix := range ignoredDevicePrefixes {
		if strings.HasPrefix(strings.ToLower(ifName), prefix) {
			return true
		}
	}
	return false
}

func scanInterfaceStats(netStatsFile string) ([]info.InterfaceStats, error) {
	file, err := os.Open(netStatsFile)
	if err != nil {
		return nil, fmt.Errorf("failure opening %s: %v", netStatsFile, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Discard header lines
	for i := 0; i < 2; i++ {
		if b := scanner.Scan(); !b {
			return nil, scanner.Err()
		}
	}

	stats := []info.InterfaceStats{}
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.Replace(line, ":", "", -1)

		fields := strings.Fields(line)
		// If the format of the  line is invalid then don't trust any of the stats
		// in this file.
		if len(fields) != 17 {
			return nil, fmt.Errorf("invalid interface stats line: %v", line)
		}

		devName := fields[0]
		if isIgnoredDevice(devName) {
			continue
		}

		i := info.InterfaceStats{
			Name: devName,
		}

		statFields := append(fields[1:5], fields[9:13]...)
		statPointers := []*uint64{
			&i.RxBytes, &i.RxPackets, &i.RxErrors, &i.RxDropped,
			&i.TxBytes, &i.TxPackets, &i.TxErrors, &i.TxDropped,
		}

		err := setInterfaceStatValues(statFields, statPointers)
		if err != nil {
			return nil, fmt.Errorf("cannot parse interface stats (%v): %v", err, line)
		}

		stats = append(stats, i)
	}

	return stats, nil
}

func setInterfaceStatValues(fields []string, pointers []*uint64) error {
	for i, v := range fields {
		val, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return err
		}
		*pointers[i] = val
	}
	return nil
}

func tcpStatsFromProc(rootFs string, pid int, file string) (info.TcpStat, error) {
	tcpStatsFile := path.Join(rootFs, "proc", strconv.Itoa(pid), file)

	tcpStats, err := scanTCPStats(tcpStatsFile)
	if err != nil {
		return tcpStats, fmt.Errorf("couldn't read tcp stats: %v", err)
	}

	return tcpStats, nil
}

func advancedTCPStatsFromProc(rootFs string, pid int, file1, file2 string) (info.TcpAdvancedStat, error) {
	var advancedStats info.TcpAdvancedStat
	var err error

	netstatFile := path.Join(rootFs, "proc", strconv.Itoa(pid), file1)
	err = scanAdvancedTCPStats(&advancedStats, netstatFile)
	if err != nil {
		return advancedStats, err
	}

	snmpFile := path.Join(rootFs, "proc", strconv.Itoa(pid), file2)
	err = scanAdvancedTCPStats(&advancedStats, snmpFile)
	if err != nil {
		return advancedStats, err
	}

	return advancedStats, nil
}

func scanAdvancedTCPStats(advancedStats *info.TcpAdvancedStat, advancedTCPStatsFile string) error {
	data, err := os.ReadFile(advancedTCPStatsFile)
	if err != nil {
		return fmt.Errorf("failure opening %s: %v", advancedTCPStatsFile, err)
	}

	reader := strings.NewReader(string(data))
	scanner := bufio.NewScanner(reader)
	scanner.Split(bufio.ScanLines)

	advancedTCPStats := make(map[string]interface{})
	for scanner.Scan() {
		nameParts := strings.Split(scanner.Text(), " ")
		scanner.Scan()
		valueParts := strings.Split(scanner.Text(), " ")
		// Remove trailing :. and ignore non-tcp
		protocol := nameParts[0][:len(nameParts[0])-1]
		if protocol != "TcpExt" && protocol != "Tcp" {
			continue
		}
		if len(nameParts) != len(valueParts) {
			return fmt.Errorf("mismatch field count mismatch in %s: %s",
				advancedTCPStatsFile, protocol)
		}
		for i := 1; i < len(nameParts); i++ {
			if strings.Contains(valueParts[i], "-") {
				vInt64, err := strconv.ParseInt(valueParts[i], 10, 64)
				if err != nil {
					return fmt.Errorf("decode value: %s to int64 error: %s", valueParts[i], err)
				}
				advancedTCPStats[nameParts[i]] = vInt64
			} else {
				vUint64, err := strconv.ParseUint(valueParts[i], 10, 64)
				if err != nil {
					return fmt.Errorf("decode value: %s to uint64 error: %s", valueParts[i], err)
				}
				advancedTCPStats[nameParts[i]] = vUint64
			}
		}
	}

	b, err := json.Marshal(advancedTCPStats)
	if err != nil {
		return err
	}

	err = json.Unmarshal(b, advancedStats)
	if err != nil {
		return err
	}

	return scanner.Err()
}

func scanTCPStats(tcpStatsFile string) (info.TcpStat, error) {
	var stats info.TcpStat

	data, err := os.ReadFile(tcpStatsFile)
	if err != nil {
		return stats, fmt.Errorf("failure opening %s: %v", tcpStatsFile, err)
	}

	tcpStateMap := map[string]uint64{
		"01": 0, // ESTABLISHED
		"02": 0, // SYN_SENT
		"03": 0, // SYN_RECV
		"04": 0, // FIN_WAIT1
		"05": 0, // FIN_WAIT2
		"06": 0, // TIME_WAIT
		"07": 0, // CLOSE
		"08": 0, // CLOSE_WAIT
		"09": 0, // LAST_ACK
		"0A": 0, // LISTEN
		"0B": 0, // CLOSING
	}

	reader := strings.NewReader(string(data))
	scanner := bufio.NewScanner(reader)

	scanner.Split(bufio.ScanLines)

	// Discard header line
	if b := scanner.Scan(); !b {
		return stats, scanner.Err()
	}

	for scanner.Scan() {
		line := scanner.Text()

		state := strings.Fields(line)
		// TCP state is the 4th field.
		// Format: sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt  uid timeout inode
		tcpState := state[3]
		_, ok := tcpStateMap[tcpState]
		if !ok {
			return stats, fmt.Errorf("invalid TCP stats line: %v", line)
		}
		tcpStateMap[tcpState]++
	}

	stats = info.TcpStat{
		Established: tcpStateMap["01"],
		SynSent:     tcpStateMap["02"],
		SynRecv:     tcpStateMap["03"],
		FinWait1:    tcpStateMap["04"],
		FinWait2:    tcpStateMap["05"],
		TimeWait:    tcpStateMap["06"],
		Close:       tcpStateMap["07"],
		CloseWait:   tcpStateMap["08"],
		LastAck:     tcpStateMap["09"],
		Listen:      tcpStateMap["0A"],
		Closing:     tcpStateMap["0B"],
	}

	return stats, nil
}

func udpStatsFromProc(rootFs string, pid int, file string) (info.UdpStat, error) {
	var err error
	var udpStats info.UdpStat

	udpStatsFile := path.Join(rootFs, "proc", strconv.Itoa(pid), file)

	r, err := os.Open(udpStatsFile)
	if err != nil {
		return udpStats, fmt.Errorf("failure opening %s: %v", udpStatsFile, err)
	}

	udpStats, err = scanUDPStats(r)
	if err != nil {
		return udpStats, fmt.Errorf("couldn't read udp stats: %v", err)
	}

	return udpStats, nil
}

func scanUDPStats(r io.Reader) (info.UdpStat, error) {
	var stats info.UdpStat

	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)

	// Discard header line
	if b := scanner.Scan(); !b {
		return stats, scanner.Err()
	}

	listening := uint64(0)
	dropped := uint64(0)
	rxQueued := uint64(0)
	txQueued := uint64(0)

	for scanner.Scan() {
		line := scanner.Text()
		// Format: sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt  uid timeout inode ref pointer drops

		listening++

		fs := strings.Fields(line)
		if len(fs) != 13 {
			continue
		}

		rx, tx := uint64(0), uint64(0)
		fmt.Sscanf(fs[4], "%X:%X", &rx, &tx)
		rxQueued += rx
		txQueued += tx

		d, err := strconv.Atoi(string(fs[12]))
		if err != nil {
			continue
		}
		dropped += uint64(d)
	}

	stats = info.UdpStat{
		Listen:   listening,
		Dropped:  dropped,
		RxQueued: rxQueued,
		TxQueued: txQueued,
	}

	return stats, nil
}

func (h *Handler) GetProcesses() ([]int, error) {
	pids, err := h.cgroupManager.GetPids()
	if err != nil {
		return nil, err
	}
	return pids, nil
}

// Convert libcontainer stats to info.ContainerStats.
func setCPUStats(s *cgroups.Stats, ret *info.ContainerStats, withPerCPU bool) {
	ret.Cpu.Usage.User = s.CpuStats.CpuUsage.UsageInUsermode
	ret.Cpu.Usage.System = s.CpuStats.CpuUsage.UsageInKernelmode
	ret.Cpu.Usage.Total = s.CpuStats.CpuUsage.TotalUsage
	ret.Cpu.CFS.Periods = s.CpuStats.ThrottlingData.Periods
	ret.Cpu.CFS.ThrottledPeriods = s.CpuStats.ThrottlingData.ThrottledPeriods
	ret.Cpu.CFS.ThrottledTime = s.CpuStats.ThrottlingData.ThrottledTime

	if !withPerCPU {
		return
	}
	if len(s.CpuStats.CpuUsage.PercpuUsage) == 0 {
		// libcontainer's 'GetStats' can leave 'PercpuUsage' nil if it skipped the
		// cpuacct subsystem.
		return
	}
	ret.Cpu.Usage.PerCpu = s.CpuStats.CpuUsage.PercpuUsage
}

func setDiskIoStats(s *cgroups.Stats, ret *info.ContainerStats) {
	ret.DiskIo.IoServiceBytes = diskStatsCopy(s.BlkioStats.IoServiceBytesRecursive)
	ret.DiskIo.IoServiced = diskStatsCopy(s.BlkioStats.IoServicedRecursive)
	ret.DiskIo.IoQueued = diskStatsCopy(s.BlkioStats.IoQueuedRecursive)
	ret.DiskIo.Sectors = diskStatsCopy(s.BlkioStats.SectorsRecursive)
	ret.DiskIo.IoServiceTime = diskStatsCopy(s.BlkioStats.IoServiceTimeRecursive)
	ret.DiskIo.IoWaitTime = diskStatsCopy(s.BlkioStats.IoWaitTimeRecursive)
	ret.DiskIo.IoMerged = diskStatsCopy(s.BlkioStats.IoMergedRecursive)
	ret.DiskIo.IoTime = diskStatsCopy(s.BlkioStats.IoTimeRecursive)
}

func setMemoryStats(s *cgroups.Stats, ret *info.ContainerStats) {
	ret.Memory.Usage = s.MemoryStats.Usage.Usage
	ret.Memory.MaxUsage = s.MemoryStats.Usage.MaxUsage
	ret.Memory.Failcnt = s.MemoryStats.Usage.Failcnt
	ret.Memory.KernelUsage = s.MemoryStats.KernelUsage.Usage

	if cgroups.IsCgroup2UnifiedMode() {
		ret.Memory.Cache = s.MemoryStats.Stats["file"]
		ret.Memory.RSS = s.MemoryStats.Stats["anon"]
		ret.Memory.Swap = s.MemoryStats.SwapUsage.Usage - s.MemoryStats.Usage.Usage
		ret.Memory.MappedFile = s.MemoryStats.Stats["file_mapped"]
		klog.Infof("ihol3 cgroups.IsCgroup2UnifiedMode()")
		klog.Infof("ihol3 s.MemoryStats.SwapUsage.Usage=%d", s.MemoryStats.SwapUsage.Usage)
		klog.Infof("ihol3 s.MemoryStats.Usage.Usage=%d", s.MemoryStats.Usage.Usage)
		klog.Infof("ihol3 ret.Memory.Swap=%d", ret.Memory.Swap)
	} else if s.MemoryStats.UseHierarchy {
		ret.Memory.Cache = s.MemoryStats.Stats["total_cache"]
		ret.Memory.RSS = s.MemoryStats.Stats["total_rss"]
		ret.Memory.Swap = s.MemoryStats.Stats["total_swap"]
		ret.Memory.MappedFile = s.MemoryStats.Stats["total_mapped_file"]
		klog.Infof("ihol3 else if s.MemoryStats.UseHierarchy")
		klog.Infof(`ihol3 s.MemoryStats.Stats["total_swap"]=%d`, s.MemoryStats.Stats["total_swap"])
	} else {
		ret.Memory.Cache = s.MemoryStats.Stats["cache"]
		ret.Memory.RSS = s.MemoryStats.Stats["rss"]
		ret.Memory.Swap = s.MemoryStats.Stats["swap"]
		ret.Memory.MappedFile = s.MemoryStats.Stats["mapped_file"]
		klog.Infof("ihol3 else ")
		klog.Infof(`ihol3 s.MemoryStats.Stats["swap"]=%d`, s.MemoryStats.Stats["swap"])
	}
	if v, ok := s.MemoryStats.Stats["pgfault"]; ok {
		ret.Memory.ContainerData.Pgfault = v
		ret.Memory.HierarchicalData.Pgfault = v
	}
	if v, ok := s.MemoryStats.Stats["pgmajfault"]; ok {
		ret.Memory.ContainerData.Pgmajfault = v
		ret.Memory.HierarchicalData.Pgmajfault = v
	}

	inactiveFileKeyName := "total_inactive_file"
	if cgroups.IsCgroup2UnifiedMode() {
		inactiveFileKeyName = "inactive_file"
	}

	activeFileKeyName := "total_active_file"
	if cgroups.IsCgroup2UnifiedMode() {
		activeFileKeyName = "active_file"
	}

	if v, ok := s.MemoryStats.Stats[activeFileKeyName]; ok {
		ret.Memory.TotalActiveFile = v
	}

	workingSet := ret.Memory.Usage
	if v, ok := s.MemoryStats.Stats[inactiveFileKeyName]; ok {
		ret.Memory.TotalInactiveFile = v
		if workingSet < v {
			workingSet = 0
		} else {
			workingSet -= v
		}
	}
	ret.Memory.WorkingSet = workingSet
}

func setCPUSetStats(s *cgroups.Stats, ret *info.ContainerStats) {
	ret.CpuSet.MemoryMigrate = s.CPUSetStats.MemoryMigrate
}

func getNumaStats(memoryStats map[uint8]uint64) map[uint8]uint64 {
	stats := make(map[uint8]uint64, len(memoryStats))
	for node, usage := range memoryStats {
		stats[node] = usage
	}
	return stats
}

func setMemoryNumaStats(s *cgroups.Stats, ret *info.ContainerStats) {
	ret.Memory.ContainerData.NumaStats.File = getNumaStats(s.MemoryStats.PageUsageByNUMA.File.Nodes)
	ret.Memory.ContainerData.NumaStats.Anon = getNumaStats(s.MemoryStats.PageUsageByNUMA.Anon.Nodes)
	ret.Memory.ContainerData.NumaStats.Unevictable = getNumaStats(s.MemoryStats.PageUsageByNUMA.Unevictable.Nodes)

	ret.Memory.HierarchicalData.NumaStats.File = getNumaStats(s.MemoryStats.PageUsageByNUMA.Hierarchical.File.Nodes)
	ret.Memory.HierarchicalData.NumaStats.Anon = getNumaStats(s.MemoryStats.PageUsageByNUMA.Hierarchical.Anon.Nodes)
	ret.Memory.HierarchicalData.NumaStats.Unevictable = getNumaStats(s.MemoryStats.PageUsageByNUMA.Hierarchical.Unevictable.Nodes)
}

func setHugepageStats(s *cgroups.Stats, ret *info.ContainerStats) {
	ret.Hugetlb = make(map[string]info.HugetlbStats)
	for k, v := range s.HugetlbStats {
		ret.Hugetlb[k] = info.HugetlbStats{
			Usage:    v.Usage,
			MaxUsage: v.MaxUsage,
			Failcnt:  v.Failcnt,
		}
	}
}

// read from pids path not cpu
func setThreadsStats(s *cgroups.Stats, ret *info.ContainerStats) {
	if s != nil {
		ret.Processes.ThreadsCurrent = s.PidsStats.Current
		ret.Processes.ThreadsMax = s.PidsStats.Limit
	}
}

func newContainerStats(cgroupStats *cgroups.Stats, includedMetrics container.MetricSet) *info.ContainerStats {
	ret := &info.ContainerStats{
		Timestamp: time.Now(),
	}

	if s := cgroupStats; s != nil {
		setCPUStats(s, ret, includedMetrics.Has(container.PerCpuUsageMetrics))
		if includedMetrics.Has(container.DiskIOMetrics) {
			setDiskIoStats(s, ret)
		}
		setMemoryStats(s, ret)
		if includedMetrics.Has(container.MemoryNumaMetrics) {
			setMemoryNumaStats(s, ret)
		}
		if includedMetrics.Has(container.HugetlbUsageMetrics) {
			setHugepageStats(s, ret)
		}
		if includedMetrics.Has(container.CPUSetMetrics) {
			setCPUSetStats(s, ret)
		}
	}
	return ret
}
