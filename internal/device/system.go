package device

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// OSInfo describes the operating system.
type OSInfo struct {
	Platform string `json:"platform"` // linux, darwin, windows
	Distro   string `json:"distro,omitempty"`
	Release  string `json:"release,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	Arch     string `json:"arch"`
	// Container is true inside Docker, Podman, or an LXC guest. Containers
	// share the host's /sys, so hardware probing there describes the host
	// rather than the reachable device; every consumer must know the
	// difference before routing work here.
	Container bool `json:"container,omitempty"`
}

// Hardware describes what the machine physically has, which is what drives
// device-affinity routing (DESIGN.md §7).
type Hardware struct {
	CPUs      int      `json:"cpus"`
	CPUModel  string   `json:"cpu_model,omitempty"`
	MemoryGB  float64  `json:"memory_gb,omitempty"`
	GPUs      []string `json:"gpus,omitempty"`
	HasScreen bool     `json:"has_screen"`
}

// DetectOS gathers operating system identity.
func DetectOS() OSInfo {
	info := OSInfo{
		Platform:  runtime.GOOS,
		Arch:      runtime.GOARCH,
		Kernel:    kernelVersion(),
		Container: InContainer(),
	}

	switch runtime.GOOS {
	case "linux":
		rel := parseOSRelease("/etc/os-release")
		info.Distro = firstNonEmpty(rel["ID"], rel["NAME"])
		info.Release = firstNonEmpty(rel["VERSION_ID"], rel["VERSION"])
	case "darwin":
		info.Distro = "macos"
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			info.Release = strings.TrimSpace(string(out))
		}
	case "windows":
		info.Distro = "windows"
	}
	return info
}

// parseOSRelease reads the freedesktop os-release key=value format.
func parseOSRelease(path string) map[string]string {
	out := map[string]string{}

	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		// Values may be quoted; unquote without a full shell parser.
		out[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return out
}

func kernelVersion() string {
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// DetectHardware gathers CPU, memory, GPU, and display presence.
func DetectHardware() Hardware {
	hw := Hardware{
		CPUs:      runtime.NumCPU(),
		CPUModel:  cpuModel(),
		MemoryGB:  memoryGB(),
		GPUs:      detectGPUs(),
		HasScreen: hasScreen(),
	}
	return hw
}

func cpuModel() string {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/cpuinfo")
		if err != nil {
			return ""
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), ":")
			if !ok {
				continue
			}
			switch strings.TrimSpace(key) {
			case "model name", "Model":
				return strings.TrimSpace(value)
			}
		}
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
}

func memoryGB() float64 {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), ":")
			if !ok || strings.TrimSpace(key) != "MemTotal" {
				continue
			}
			fields := strings.Fields(value)
			if len(fields) == 0 {
				return 0
			}
			kb, err := strconv.ParseFloat(fields[0], 64)
			if err != nil {
				return 0
			}
			return round1(kb / 1024 / 1024)
		}
	case "darwin":
		out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
		if err != nil {
			return 0
		}
		bytes, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if err != nil {
			return 0
		}
		return round1(bytes / 1024 / 1024 / 1024)
	}
	return 0
}

// detectGPUs reads sysfs directly rather than shelling out to lspci, which is
// not installed on a minimal system.
func detectGPUs() []string {
	if runtime.GOOS != "linux" {
		return nil
	}

	var gpus []string
	seen := map[string]bool{}

	// DRM cards are the reliable signal for a usable graphics device.
	entries, err := filepath.Glob("/sys/class/drm/card[0-9]/device/vendor")
	if err == nil {
		for _, path := range entries {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			vendor := vendorName(strings.TrimSpace(string(data)))
			if vendor != "" && !seen[vendor] {
				seen[vendor] = true
				gpus = append(gpus, vendor)
			}
		}
	}

	// nvidia-smi is absent on a bare system, but the proc entry is not.
	if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil && !seen["nvidia"] {
		gpus = append(gpus, "nvidia")
	}
	return gpus
}

// vendorName maps PCI vendor ids to names. These four cover essentially all
// consumer and workstation graphics.
func vendorName(id string) string {
	switch strings.ToLower(id) {
	case "0x10de":
		return "nvidia"
	case "0x1002":
		return "amd"
	case "0x8086":
		return "intel"
	case "0x1af4":
		return "virtio"
	}
	return ""
}

// InContainer reports whether this process runs inside a container.
//
// This matters beyond labelling: container runtimes bind-mount the host's
// /sys, so DRM and PCI probing describes hardware the container cannot
// actually use. Anything reading Hardware must account for that.
func InContainer() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	// Runtime-specific marker files are the cheapest reliable signal.
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	// cgroup membership catches runtimes that leave no marker file.
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		content := string(data)
		for _, needle := range []string{"docker", "containerd", "lxc", "kubepods", "libpod"} {
			if strings.Contains(content, needle) {
				return true
			}
		}
	}
	return false
}

// hasScreen reports whether a graphical session is reachable. Headless servers
// and containers must not be handed tasks that need a display.
func hasScreen() bool {
	switch runtime.GOOS {
	case "linux":
		if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
			return true
		}
		// Inside a container the DRM nodes below belong to the host, so a
		// connected output there says nothing about this device.
		if InContainer() {
			return false
		}
		// A connected DRM output means a screen exists even with no session.
		matches, err := filepath.Glob("/sys/class/drm/card*/status")
		if err != nil {
			return false
		}
		for _, path := range matches {
			if data, err := os.ReadFile(path); err == nil {
				if strings.TrimSpace(string(data)) == "connected" {
					return true
				}
			}
		}
		return false
	case "darwin", "windows":
		return true
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
