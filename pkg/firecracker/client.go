package firecracker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"

	v1 "github.com/loopholelabs/architekt/pkg/api/http/firecracker/v1"
)

var (
	ErrCouldNotSetBootSource        = errors.New("could not set boot source")
	ErrCouldNotSetDrive             = errors.New("could not set drive")
	ErrCouldNotSetMachineConfig     = errors.New("could not set machine config")
	ErrCouldNotSetNetworkInterfaces = errors.New("could not set network interfaces")
	ErrCouldNotStartInstance        = errors.New("could not start instance")
	ErrCouldNotStopInstance         = errors.New("could not stop instance")
)

func putJSON(client *http.Client, body any, resource string) error {
	p, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPut, "http://localhost/"+resource, bytes.NewReader(p))
	if err != nil {
		return err
	}

	res, err := client.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode >= 300 {
		b, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}

		return errors.New(string(b))
	}

	return nil
}

func StartVM(
	client *http.Client,

	initramfsPath string,
	kernelPath string,
	diskPath string,

	cpuCount int,
	memorySize int,

	hostInterface string,
	hostMAC string,
) error {
	if err := putJSON(
		client,
		&v1.BootSource{
			InitrdPath:      initramfsPath,
			KernelImagePath: kernelPath,
			BootArgs:        "console=ttyS0 panic=1 pci=off modules=ext4 rootfstype=ext4 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd rootflags=rw",
		},
		"boot-source",
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotSetBootSource, err)
	}

	if err := putJSON(
		client,
		&v1.Drive{
			DriveID:      "root",
			PathOnHost:   diskPath,
			IsRootDevice: true,
			IsReadOnly:   false,
		},
		path.Join("drives", "root"),
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotSetDrive, err)
	}

	if err := putJSON(
		client,
		&v1.MachineConfig{
			VCPUCount:  cpuCount,
			MemSizeMib: memorySize,
		},
		"machine-config",
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotSetMachineConfig, err)
	}

	if err := putJSON(
		client,
		&v1.NetworkInterface{
			IfaceID:     hostInterface,
			GuestMAC:    hostMAC,
			HostDevName: hostInterface,
		},
		path.Join("network-interfaces", hostInterface),
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotSetNetworkInterfaces, err)
	}

	if err := putJSON(
		client,
		&v1.Action{
			ActionType: "InstanceStart",
		},
		"actions",
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotStartInstance, err)
	}

	return nil
}

func StopVM(
	client *http.Client,
) error {
	if err := putJSON(
		client,
		&v1.Action{
			ActionType: "SendCtrlAltDel",
		},
		"actions",
	); err != nil {
		return fmt.Errorf("%w: %s", ErrCouldNotStopInstance, err)
	}

	return nil
}
