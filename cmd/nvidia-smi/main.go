package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/run-ai/fake-gpu-operator/internal/common/constants"
	"github.com/run-ai/fake-gpu-operator/internal/common/topology"
)

type nvidiaSmiArgs struct {
	GpuProduct  string
	GpuUsedMem  float32
	GpuTotalMem int
	GpuUtil     int
	GpuIdx      int
	ProcessName string
	MigProfile  string
	MigGiId     int
}

type config struct {
	Debug bool
}

var conf config = config{}

func main() {
	flag.BoolVar(&conf.Debug, "debug", false, "enable debug mode")
	flag.Parse()

	if conf.Debug {
		fmt.Println("Debug mode enabled")
	}

	args, summary, errs := getNvidiaSmiArgs()

	if len(args) == 0 {
		fmt.Println(summary)
		printErrors(errs)
		os.Exit(1)
	}

	printArgs(args)
	printErrors(errs)
}

func printErrors(errs []error) {
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, err)
	}
}

// getNvidiaSmiArgs returns the per-GPU rows to render. When no devices can be
// obtained it returns an empty slice plus a user-facing summary line describing
// the failure (printed to stdout), with the underlying causes in errs (stderr).
func getNvidiaSmiArgs() (args []nvidiaSmiArgs, summary string, errs []error) {

	nodeName := os.Getenv(constants.EnvNodeName)
	if conf.Debug {
		fmt.Printf("Node name: %s\n", nodeName)
	}

	topologyUrl := "http://topology-server.gpu-operator/topology/nodes/" + nodeName
	if conf.Debug {
		fmt.Printf("Requesting topology from: %s\n", topologyUrl)
	}
	// Topology is required to render any device, so fetch/status/decode failures
	// are fatal: accumulate the error and return no devices.
	resp, err := http.Get(topologyUrl)
	if err != nil {
		return nil, "NVIDIA-SMI has failed because it couldn't communicate with the topology server.", append(errs, fmt.Errorf("fetching topology from %s: %w", topologyUrl, err))
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing topology response body: %v\n", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "No devices were found", append(errs, fmt.Errorf("topology server %s returned %s: %s", topologyUrl, resp.Status, strings.TrimSpace(string(body))))
	}

	var nodeTopology topology.NodeTopology
	if err := json.NewDecoder(resp.Body).Decode(&nodeTopology); err != nil {
		return nil, "Failed to parse devices data", append(errs, fmt.Errorf("decoding topology from %s: %w", topologyUrl, err))
	}
	if conf.Debug {
		fmt.Printf("Received topology: %+v\n", nodeTopology)
	}

	migProfile := os.Getenv("MOCK_MIG_PROFILE")
	if conf.Debug {
		fmt.Printf("MIG profile: %s\n", migProfile)
	}

	if migProfile != "" {
		return getMigArgs(nodeTopology, migProfile)
	}

	gpuPortion := 1.0
	numOfGpus := os.Getenv("RUNAI_NUM_OF_GPUS")
	if numOfGpus != "" {
		if parsed, err := strconv.ParseFloat(numOfGpus, 32); err != nil {
			errs = append(errs, fmt.Errorf("parsing RUNAI_NUM_OF_GPUS %q: %w", numOfGpus, err))
		} else {
			gpuPortion = parsed
		}
		if conf.Debug {
			fmt.Printf("GPU portion from RUNAI_NUM_OF_GPUS: %f\n", gpuPortion)
		}
	}
	gpuTotalMem := int(float64(nodeTopology.GpuMemory) * gpuPortion)

	currentPodName := os.Getenv("HOSTNAME")
	currentPodUuid := os.Getenv("POD_UUID")
	if conf.Debug {
		fmt.Printf("Current pod name: %s, UUID: %s\n", currentPodName, currentPodUuid)
	}

	processName := readProcessName()

	var allArgs []nvidiaSmiArgs
	for idx, gpu := range nodeTopology.Gpus {
		matched := false
		if gpu.Status.AllocatedBy.Pod == currentPodName {
			matched = true
		} else {
			for podUuid := range gpu.Status.PodGpuUsageStatus {
				if string(podUuid) == currentPodUuid {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		if conf.Debug {
			fmt.Printf("Found GPU %d allocated to pod %s\n", idx, currentPodName)
		}
		allArgs = append(allArgs, nvidiaSmiArgs{
			GpuProduct:  nodeTopology.GpuProduct,
			GpuTotalMem: gpuTotalMem,
			GpuUsedMem:  float32(gpu.Status.PodGpuUsageStatus.FbUsed(nodeTopology.GpuMemory)) * float32(gpuPortion),
			GpuUtil:     gpu.Status.PodGpuUsageStatus.Utilization(),
			GpuIdx:      idx,
			ProcessName: processName,
		})
	}

	if len(allArgs) == 0 {
		allArgs = append(allArgs, nvidiaSmiArgs{
			GpuProduct:  nodeTopology.GpuProduct,
			GpuTotalMem: gpuTotalMem,
			ProcessName: processName,
		})
	}

	return allArgs, "", errs
}

func getMigArgs(nodeTopology topology.NodeTopology, migProfile string) ([]nvidiaSmiArgs, string, []error) {
	processName := readProcessName()

	migMem := 0
	parts := strings.Split(migProfile, ".")
	if len(parts) == 2 {
		memStr := strings.TrimSuffix(parts[1], "gb")
		if val, err := strconv.Atoi(memStr); err == nil {
			migMem = val * 1024
		}
	}

	giId := 0
	if len(parts) >= 1 {
		giStr := strings.TrimSuffix(parts[0], "g")
		if val, err := strconv.Atoi(giStr); err == nil {
			switch val {
			case 1:
				giId = 19
			case 2:
				giId = 14
			case 3:
				giId = 9
			case 4:
				giId = 5
			case 7:
				giId = 0
			}
		}
	}

	visibleDevices := os.Getenv("MOCK_NVIDIA_VISIBLE_DEVICES")
	deviceCount := 1
	if visibleDevices != "" {
		deviceCount = len(strings.Split(visibleDevices, ","))
	}

	var allArgs []nvidiaSmiArgs
	for i := 0; i < deviceCount; i++ {
		allArgs = append(allArgs, nvidiaSmiArgs{
			GpuProduct:  nodeTopology.GpuProduct,
			GpuTotalMem: migMem,
			GpuIdx:      i,
			ProcessName: processName,
			MigProfile:  migProfile,
			MigGiId:     giId,
		})
	}

	return allArgs, "", nil
}

func readProcessName() string {
	cmdlineFile, err := os.Open("/proc/1/cmdline")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening /proc/1/cmdline: %v\n", err)
		return "unknown"
	}
	defer func() {
		if err := cmdlineFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing cmdline file: %v\n", err)
		}
	}()

	cmdlineBytes := make([]byte, 50)
	if _, err := cmdlineFile.Read(cmdlineBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading /proc/1/cmdline: %v\n", err)
		return "unknown"
	}

	return string(bytes.Trim(cmdlineBytes, "\x00"))
}

func printArgs(allArgs []nvidiaSmiArgs) {
	if conf.Debug {
		fmt.Println("Printing nvidia-smi output")
	}

	isMig := len(allArgs) > 0 && allArgs[0].MigProfile != ""
	migMode := "N/A"
	if isMig {
		migMode = "Enabled"
	}

	fmt.Println(time.Now().Format(time.ANSIC))
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle("NVIDIA-SMI 470.129.06   Driver Version: 470.129.06   CUDA Version: 11.4")
	t.AppendSeparator()
	t.AppendRow(table.Row{"GPU  Name        Persistence-M", "Bus-Id        Disp.A", "Volatile Uncorr. ECC"})
	t.AppendRow(table.Row{"Fan  Temp  Perf  Pwr:Usage/Cap", "        Memory-Usage", "GPU-Util  Compute M."})
	t.AppendRow(table.Row{"", "", "              MIG M."})
	t.AppendSeparator()

	if isMig {
		t.AppendRow(table.Row{fmt.Sprintf("%s  %s%s", sizeString("0", 3, true), sizeString(allArgs[0].GpuProduct, 12, false), sizeString("Off", 13, true)), fmt.Sprintf("%s %s", sizeString("00000001:00:00.0", 16, false), sizeString("Off", 3, true)), sizeString("On", 20, true)})
		t.AppendRow(table.Row{"N/A   33C    P0    72W / 700W", sizeString(fmt.Sprintf("0MiB / %dMiB", allArgs[0].GpuTotalMem), 20, true), fmt.Sprintf("%s %s", sizeString("0%", 8, true), sizeString("Default", 11, true))})
		t.AppendRow(table.Row{"", "", sizeString(migMode, 20, true)})
		t.AppendSeparator()
	} else {
		for _, args := range allArgs {
			t.AppendRow(table.Row{fmt.Sprintf("%s  %s%s", sizeString(strconv.Itoa(args.GpuIdx), 3, true), sizeString(args.GpuProduct, 12, false), sizeString("Off", 13, true)), fmt.Sprintf("%s %s", sizeString("00000001:00:00.0", 16, false), sizeString("Off", 3, true)), sizeString("Off", 20, true)})
			t.AppendRow(table.Row{"N/A   33C    P8    11W /  70W", sizeString(fmt.Sprintf("%dMiB / %dMiB", int(args.GpuUsedMem), args.GpuTotalMem), 20, true), fmt.Sprintf("%s %s", sizeString(strconv.Itoa(args.GpuUtil)+"%", 8, true), sizeString("Default", 11, true))})
			t.AppendRow(table.Row{"", "", sizeString("N/A", 20, true)})
			t.AppendSeparator()
		}
	}
	t.Render()

	if isMig {
		fmt.Printf("\n")
		t = table.NewWriter()
		t.SetOutputMirror(os.Stdout)
		t.AppendRow(table.Row{"MIG devices:"})
		t.AppendRow(table.Row{" GPU  GI  CI  MIG         Memory-Usage       Vol       Shared        "})
		t.AppendRow(table.Row{"      ID  ID  Dev          BAR1-Usage        SM  Unc  CE ENC DEC OFA JPG"})
		t.AppendRow(table.Row{"                                             ECC                       "})
		t.AppendSeparator()
		for _, args := range allArgs {
			t.AppendRow(table.Row{fmt.Sprintf("   0  %s   0  %s   %s  %s   %s",
				sizeString(strconv.Itoa(args.MigGiId), 3, true),
				sizeString(strconv.Itoa(args.GpuIdx), 3, true),
				sizeString(fmt.Sprintf("%dMiB / %dMiB", int(args.GpuUsedMem), args.GpuTotalMem), 20, true),
				sizeString("16", 3, true),
				sizeString("0", 3, true),
			)})
		}
		t.AppendSeparator()
		t.Render()
	}

	fmt.Printf("\n")

	t = table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendRow(table.Row{"Processes:"})
	t.AppendRow(table.Row{" GPU   GI   CI        PID   Type   Process name                  GPU Memory "})
	t.AppendRow(table.Row{"       ID   ID                                                   Usage      "})
	t.AppendSeparator()
	for _, args := range allArgs {
		giStr := "N/A"
		if args.MigProfile != "" {
			giStr = strconv.Itoa(args.MigGiId)
		}
		ciStr := "N/A"
		if args.MigProfile != "" {
			ciStr = "0"
		}
		processType := "G"
		if args.MigProfile != "" {
			processType = "C"
		}
		t.AppendRow(table.Row{fmt.Sprintf(" %s   %s%s%s%s   %s %s", sizeString(strconv.Itoa(args.GpuIdx), 3, true), sizeString(giStr, 5, false), sizeString(ciStr, 10, false), sizeString(strconv.Itoa(os.Getpid()), 6, false), sizeString(processType, 4, true), sizeString(args.ProcessName, 29, false), sizeString(fmt.Sprintf("%dMiB", int(args.GpuUsedMem)), 11, true))})
	}
	t.Render()
}

func sizeString(str string, size int, alignRight bool) string {
	if len(str) < size {
		if alignRight {
			str = strings.Repeat(" ", size-len(str)) + str
		} else {
			str = str + strings.Repeat(" ", size-len(str))
		}
	}

	if len(str) > size {
		str = str[:size-2] + ".."
	}

	return str[:size]
}
