package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/loopholelabs/architekt/pkg/firecracker"
)

func main() {
	pwd, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	firecrackerSocketPath := flag.String("firecracker-socket-path", "firecracker.sock", "Firecracker socket path")

	initramfsPath := flag.String("initramfs-path", "out/template/architekt.initramfs", "initramfs path")
	kernelPath := flag.String("kernel-path", "out/template/architekt.kernel", "Kernel path")
	diskPath := flag.String("disk-path", "out/template/architekt.disk", "Disk path")
	statePath := flag.String("state-path", "out/template/architekt.state", "State path")
	memoryPath := flag.String("memory-path", "out/template/architekt.memory", "Memory path")

	cpuCount := flag.Int("cpu-count", 1, "CPU count")
	memorySize := flag.Int("memory-size", 1024, "Memory size (in MB)")

	hostInterface := flag.String("host-interface", "vm0", "Host interface name")
	hostMAC := flag.String("host-mac", "02:0e:d9:fd:68:3d", "Host MAC address")

	vsockPath := flag.String("vsock-path", filepath.Join(pwd, "vsock.sock"), "VSock path (must be absolute; will be recreated at this place when restoring)")
	vsockCID := flag.Int("vsock-cid", 3, "VSock CID")

	start := flag.Bool("start", false, "Whether to start the VM")
	stop := flag.Bool("stop", false, "Whether to stop the VM")
	createSnapshot := flag.Bool("create-snapshot", false, "Whether to create a VM snapshot")
	resumeSnapshot := flag.Bool("resume-snapshot", false, "Whether to resume a VM snapshot")
	flushSnapshot := flag.Bool("flush-snapshot", false, "Whether to flush a VM snapshot")

	flag.Parse()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return net.Dial("unix", *firecrackerSocketPath)
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

			*vsockPath,
			*vsockCID,
		); err != nil {
			panic(err)
		}
	}

	if *createSnapshot {
		if err := firecracker.CreateSnapshot(
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

	if *resumeSnapshot {
		if err := firecracker.ResumeSnapshot(
			client,

			*statePath,
			*memoryPath,
		); err != nil {
			panic(err)
		}
	}

	if *flushSnapshot {
		if err := firecracker.FlushSnapshot(
			client,

			*statePath,
		); err != nil {
			panic(err)
		}
	}
}
