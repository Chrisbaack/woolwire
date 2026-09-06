package hosting

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

type HardwareProfile struct {
	Arch         string `json:"arch"`
	OS           string `json:"os"`
	CPUCores     int    `json:"cpu_cores"`
	TotalRAMMB   int64  `json:"total_ram_mb"`
	HasNVIDIAGPU bool   `json:"has_nvidia_gpu"`
	GPUName      string `json:"gpu_name,omitempty"`
}

func DetectHardware() HardwareProfile {
	profile := HardwareProfile{
		Arch:     runtime.GOARCH,
		OS:       runtime.GOOS,
		CPUCores: runtime.NumCPU(),
	}

	// Detect Total RAM on Linux
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		lines := strings.Split(string(b), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						profile.TotalRAMMB = kb / 1024
					}
				}
				break
			}
		}
	}

	// Detect NVIDIA GPU
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		profile.HasNVIDIAGPU = true
	} else if _, err := os.Stat("/proc/driver/nvidia/version"); err == nil {
		profile.HasNVIDIAGPU = true
	}

	if profile.HasNVIDIAGPU {
		// Attempt to extract GPU name via nvidia-smi if available
		if out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output(); err == nil {
			name := strings.TrimSpace(string(out))
			if name != "" {
				profile.GPUName = name
			}
		}
		if profile.GPUName == "" {
			profile.GPUName = "NVIDIA CUDA Compatible Device"
		}
	}

	return profile
}
