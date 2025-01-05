package fs2

import (
	"bufio"
	"errors"
	"k8s.io/klog/v2"
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	"github.com/opencontainers/runc/libcontainer/configs"
)

// numToStr converts an int64 value to a string for writing to a
// cgroupv2 files with .min, .max, .low, or .high suffix.
// The value of -1 is converted to "max" for cgroupv1 compatibility
// (which used to write -1 to remove the limit).
func numToStr(value int64) (ret string) {
	switch {
	case value == 0:
		ret = ""
	case value == -1:
		ret = "max"
	default:
		ret = strconv.FormatInt(value, 10)
	}

	return ret
}

func isMemorySet(r *configs.Resources) bool {
	return r.MemoryReservation != 0 || r.Memory != 0 || r.MemorySwap != 0
}

func setMemory(dirPath string, r *configs.Resources) error {
	if !isMemorySet(r) {
		return nil
	}

	if err := CheckMemoryUsage(dirPath, r); err != nil {
		return err
	}

	swap, err := cgroups.ConvertMemorySwapToCgroupV2Value(r.MemorySwap, r.Memory)
	if err != nil {
		return err
	}
	swapStr := numToStr(swap)
	if swapStr == "" && swap == 0 && r.MemorySwap > 0 {
		// memory and memorySwap set to the same value -- disable swap
		swapStr = "0"
	}
	// never write empty string to `memory.swap.max`, it means set to 0.
	if swapStr != "" {
		if err := cgroups.WriteFile(dirPath, "memory.swap.max", swapStr); err != nil {
			// If swap is not enabled, silently ignore setting to max or disabling it.
			if !(errors.Is(err, os.ErrNotExist) && (swapStr == "max" || swapStr == "0")) {
				return err
			}
		}
	}

	if val := numToStr(r.Memory); val != "" {
		if err := cgroups.WriteFile(dirPath, "memory.max", val); err != nil {
			return err
		}
	}

	// cgroup.Resources.KernelMemory is ignored

	if val := numToStr(r.MemoryReservation); val != "" {
		if err := cgroups.WriteFile(dirPath, "memory.low", val); err != nil {
			return err
		}
	}

	return nil
}

func statMemory(dirPath string, stats *cgroups.Stats) error {
	const file = "memory.stat"
	statsFile, err := cgroups.OpenFile(dirPath, file, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer statsFile.Close()

	sc := bufio.NewScanner(statsFile)
	for sc.Scan() {
		t, v, err := fscommon.ParseKeyValue(sc.Text())
		if err != nil {
			return &parseError{Path: dirPath, File: file, Err: err}
		}
		stats.MemoryStats.Stats[t] = v
	}
	if err := sc.Err(); err != nil {
		return &parseError{Path: dirPath, File: file, Err: err}
	}
	stats.MemoryStats.Cache = stats.MemoryStats.Stats["file"]
	// Unlike cgroup v1 which has memory.use_hierarchy binary knob,
	// cgroup v2 is always hierarchical.
	stats.MemoryStats.UseHierarchy = true

	klog.Infof("ihol3 statMemory")
	memoryUsage, err := getMemoryDataV2(dirPath, "")
	klog.Infof("ihol3 memoryUsage: %d, err: %v", memoryUsage.Usage, err)
	if err != nil {
		if errors.Is(err, unix.ENOENT) && dirPath == UnifiedMountpoint {
			// The root cgroup does not have memory.{current,max,peak}
			// so emulate those using data from /proc/meminfo and
			// /sys/fs/cgroup/memory.stat
			err = rootStatsFromMeminfo(stats)
			klog.InfoS("ihol3 rootStatsFromMeminfo() if errors.Is(err, unix.ENOENT) && dirPath == UnifiedMountpoint {", "err", err)
			return err
		}
		return err
	}
	stats.MemoryStats.Usage = memoryUsage
	swapOnlyUsage, err := getMemoryDataV2(dirPath, "swap")
	klog.Infof("ihol3 swapOnlyUsage: %d, err: %v", swapOnlyUsage.Usage, err)
	if err != nil {
		return err
	}
	stats.MemoryStats.SwapOnlyUsage = swapOnlyUsage
	swapUsage := swapOnlyUsage
	// As cgroup v1 reports SwapUsage values as mem+swap combined,
	// while in cgroup v2 swap values do not include memory,
	// report combined mem+swap for v1 compatibility.
	swapUsage.Usage += memoryUsage.Usage
	if swapUsage.Limit != math.MaxUint64 {
		swapUsage.Limit += memoryUsage.Limit
	}
	// The `MaxUsage` of mem+swap cannot simply combine mem with
	// swap. So set it to 0 for v1 compatibility.
	swapUsage.MaxUsage = 0
	stats.MemoryStats.SwapUsage = swapUsage

	return nil
}

func getMemoryDataV2(path, name string) (cgroups.MemoryData, error) {
	memoryData := cgroups.MemoryData{}

	moduleName := "memory"
	if name != "" {
		moduleName = "memory." + name
	}
	usage := moduleName + ".current"
	limit := moduleName + ".max"
	maxUsage := moduleName + ".peak"

	value, err := fscommon.GetCgroupParamUint(path, usage)
	if err != nil {
		if name != "" && os.IsNotExist(err) {
			// Ignore EEXIST as there's no swap accounting
			// if kernel CONFIG_MEMCG_SWAP is not set or
			// swapaccount=0 kernel boot parameter is given.
			return cgroups.MemoryData{}, nil
		}
		return cgroups.MemoryData{}, err
	}
	memoryData.Usage = value

	value, err = fscommon.GetCgroupParamUint(path, limit)
	if err != nil {
		return cgroups.MemoryData{}, err
	}
	memoryData.Limit = value

	// `memory.peak` since kernel 5.19
	// `memory.swap.peak` since kernel 6.5
	value, err = fscommon.GetCgroupParamUint(path, maxUsage)
	if err != nil && !os.IsNotExist(err) {
		return cgroups.MemoryData{}, err
	}
	memoryData.MaxUsage = value

	return memoryData, nil
}

func rootStatsFromMeminfo(stats *cgroups.Stats) error {
	const file = "/proc/meminfo"
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	// Fields we are interested in.
	var (
		swap_free  uint64
		swap_total uint64
	)
	mem := map[string]*uint64{
		"SwapFree":  &swap_free,
		"SwapTotal": &swap_total,
	}

	found := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 2 {
			// Should not happen.
			continue
		}
		k := parts[0]
		p, ok := mem[k]
		if !ok {
			// Unknown field -- not interested.
			continue
		}
		vStr := strings.TrimSpace(strings.TrimSuffix(parts[1], " kB"))
		*p, err = strconv.ParseUint(vStr, 10, 64)
		if err != nil {
			return &parseError{File: file, Err: errors.New("bad value for " + k)}
		}

		found++
		if found == len(mem) {
			// Got everything we need -- skip the rest.
			break
		}
	}
	if err := sc.Err(); err != nil {
		return &parseError{Path: "", File: file, Err: err}
	}

	// cgroup v1 `usage_in_bytes` reports memory usage as the sum of
	// - rss (NR_ANON_MAPPED)
	// - cache (NR_FILE_PAGES)
	// cgroup v1 reports SwapUsage values as mem+swap combined
	// cgroup v2 reports rss and cache as anon and file.
	// sum `anon` + `file` to report the same value as `usage_in_bytes` in v1.
	// sum swap usage as combined mem+swap usage for consistency as well.
	stats.MemoryStats.Usage.Usage = stats.MemoryStats.Stats["anon"] + stats.MemoryStats.Stats["file"]
	stats.MemoryStats.Usage.Limit = math.MaxUint64
	stats.MemoryStats.SwapUsage.Usage = (swap_total - swap_free) * 1024
	stats.MemoryStats.SwapUsage.Limit = math.MaxUint64
	stats.MemoryStats.SwapUsage.Usage += stats.MemoryStats.Usage.Usage
	klog.InfoS("ihol3 rootStatsFromMeminfo()", "swap usage", (swap_total-swap_free)*1024, "mem usage", stats.MemoryStats.Usage.Usage, "swap+mem", stats.MemoryStats.SwapUsage.Usage)

	return nil
}
