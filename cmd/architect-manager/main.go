package main

import (
	"context"
	"flag"
	"net"
	"net/http"

	"github.com/loopholelabs/architekt/pkg/firecracker"
)

func main() {
	firecrackerSocket := flag.String("firecracker-socket", "firecracker.sock", "Firecracker socket")

	initramfsPath := flag.String("initramfs-path", "out/template/architekt.initramfs", "initramfs path")
	kernelPath := flag.String("kernel-path", "out/template/architekt.kernel", "Kernel path")
	diskPath := flag.String("disk-path", "out/template/architekt.disk", "Disk path")
	statePath := flag.String("state-path", "out/template/architekt.state", "State path")
	memoryPath := flag.String("memory-path", "out/template/architekt.memory", "Memory path")

	cpuCount := flag.Int("cpu-count", 1, "CPU count")
	memorySize := flag.Int("memory-size", 1024, "Memory size (in MB)")

	hostInterface := flag.String("host-interface", "vm0", "Host interface name")
	hostMAC := flag.String("host-mac", "02:0e:d9:fd:68:3d", "Host MAC address")

	start := flag.Bool("start", false, "Whether to start the VM")
	stop := flag.Bool("stop", false, "Whether to stop the VM")
	snapshot := flag.Bool("snapshot", false, "Whether to snapshot the VM")

	flag.Parse()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", *firecrackerSocket)
			},
		},
	}

	if *start {
		if err := firecracker.StartVM(
			client,

			*initramfsPath,
			*kernelPath,
			*diskPath,

			*cpuCount,
			*memorySize,

			*hostInterface,
			*hostMAC,
		); err != nil {
			panic(err)
		}
	}

	if *snapshot {
		if err := firecracker.SnapshotVM(
			client,

			*statePath,
			*memoryPath,
		); err != nil {
			panic(err)
		}
	}

	if *stop {
		if err := firecracker.StopVM(client); err != nil {
			panic(err)
		}
	}
}
