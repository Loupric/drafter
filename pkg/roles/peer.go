package roles

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	iutils "github.com/loopholelabs/drafter/internal/utils"
	"github.com/loopholelabs/drafter/pkg/config"
	"github.com/loopholelabs/drafter/pkg/utils"
	"github.com/loopholelabs/silo/pkg/storage"
	"github.com/loopholelabs/silo/pkg/storage/expose"
	"github.com/loopholelabs/silo/pkg/storage/protocol"
	"github.com/loopholelabs/silo/pkg/storage/protocol/packets"
	"github.com/loopholelabs/silo/pkg/storage/sources"
	"github.com/loopholelabs/silo/pkg/storage/waitingcache"
	"golang.org/x/sys/unix"
)

var (
	ErrCouldNotGetNBDDeviceStat = errors.New("could not get NBD device stat")
)

type MigrateFromHooks struct {
	OnDeviceReceived           func(deviceID uint32, name string)
	OnDeviceExposed            func(deviceID uint32, path string)
	OnDeviceAuthorityReceived  func(deviceID uint32)
	OnDeviceMigrationCompleted func(deviceID uint32)

	OnAllDevicesReceived     func()
	OnAllMigrationsCompleted func()
}

type MigratedPeer struct {
	Wait  func() error
	Close func() error

	Resume func(
		ctx context.Context,

		resumeTimeout time.Duration,
	) (
		resumedPeer *ResumedRunner,

		errs error,
	)
}

type peerStage1 struct {
	name      string
	base      string
	blockSize uint32
}

type Peer struct {
	VMPath string

	Wait  func() error
	Close func() error

	MigrateFrom func(
		ctx context.Context,

		statePath,
		memoryPath,
		initramfsPath,
		kernelPath,
		diskPath,
		configPath string,

		stateBlockSizeStorage,
		memoryBlockSizeStorage,
		initramfsBlockSizeStorage,
		kernelBlockSizeStorage,
		diskBlockSizeStorage,
		configBlockSizeStorage uint32,

		stateBlockSizeDevice,
		memoryBlockSizeDevice,
		initramfsBlockSizeDevice,
		kernelBlockSizeDevice,
		diskBlockSizeDevice,
		configBlockSizeDevice uint64,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateFromHooks,
	) (
		migratedPeer *MigratedPeer,

		errs error,
	)
}

func StartPeer(
	hypervisorCtx context.Context,
	rescueCtx context.Context,
	hypervisorConfiguration config.HypervisorConfiguration,

	stateName string,
	memoryName string,
) (
	peer *Peer,

	errs error,
) {
	peer = &Peer{}

	_, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		hypervisorCtx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	runner, err := StartRunner(
		hypervisorCtx,
		rescueCtx,

		hypervisorConfiguration,

		stateName,
		memoryName,
	)

	// We set both of these even if we return an error since we need to have a way to wait for rescue operations to complete
	peer.Wait = runner.Wait
	peer.Close = func() error {
		if runner.Close != nil {
			if err := runner.Close(); err != nil {
				return err
			}
		}

		if peer.Wait != nil {
			if err := peer.Wait(); err != nil {
				return err
			}
		}

		return nil
	}

	if err != nil {
		panic(err)
	}

	peer.VMPath = runner.VMPath

	// We don't track this because we return the wait function
	handleGoroutinePanics(false, func() {
		if err := runner.Wait(); err != nil {
			panic(err)
		}
	})

	peer.MigrateFrom = func(
		ctx context.Context,

		statePath,
		memoryPath,
		initramfsPath,
		kernelPath,
		diskPath,
		configPath string,

		stateBlockSizeStorage,
		memoryBlockSizeStorage,
		initramfsBlockSizeStorage,
		kernelBlockSizeStorage,
		diskBlockSizeStorage,
		configBlockSizeStorage uint32,

		stateBlockSizeDevice,
		memoryBlockSizeDevice,
		initramfsBlockSizeDevice,
		kernelBlockSizeDevice,
		diskBlockSizeDevice,
		configBlockSizeDevice uint64,

		readers []io.Reader,
		writers []io.Writer,

		hooks MigrateFromHooks,
	) (
		migratedPeer *MigratedPeer,

		errs error,
	) {
		migratedPeer = &MigratedPeer{}

		// We use the background context here instead of the internal context because we want to distinguish
		// between a context cancellation from the outside and getting a response
		allDevicesReceivedCtx, cancelAllDevicesReceivedCtx := context.WithCancel(ctx)
		defer cancelAllDevicesReceivedCtx()

		allDevicesReadyCtx, cancelAllDevicesReadyCtx := context.WithCancel(ctx)
		defer cancelAllDevicesReadyCtx()

		// We don't `defer cancelProtocolCtx()` this because we cancel in the wait function
		protocolCtx, cancelProtocolCtx := context.WithCancel(ctx)

		// We overwrite this further down, but this is so that we don't leak the `protocolCtx` if we `panic()` before we set `WaitForMigrationsToComplete`
		migratedPeer.Wait = func() error {
			cancelProtocolCtx()

			return nil
		}

		internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
			ctx,
			&errs,
			utils.GetPanicHandlerHooks{},
		)
		defer wait()
		defer cancel()
		defer handlePanics(false)()

		// Use an atomic counter and `allDevicesReadyCtx` and instead of a WaitGroup so that we can `select {}` without leaking a goroutine
		var (
			receivedButNotReadyDevices atomic.Int32

			deviceCloseFuncsLock sync.Mutex
			deviceCloseFuncs     []func() error

			receivedDevicesLock sync.Mutex
			receivedDevices     []string
		)
		pro := protocol.NewProtocolRW(
			protocolCtx, // We don't track this because we return the wait function
			readers,
			writers,
			func(p protocol.Protocol, index uint32) {
				var (
					from  *protocol.FromProtocol
					local *waitingcache.WaitingCacheLocal
				)
				from = protocol.NewFromProtocol(
					index,
					func(di *packets.DevInfo) storage.StorageProvider {
						defer handlePanics(false)()

						var (
							path            = ""
							blockSizeDevice = uint64(0)
						)
						switch di.Name {
						case config.ConfigName:
							path = configPath
							blockSizeDevice = configBlockSizeDevice

						case config.DiskName:
							path = diskPath
							blockSizeDevice = diskBlockSizeDevice

						case config.InitramfsName:
							path = initramfsPath
							blockSizeDevice = initramfsBlockSizeDevice

						case config.KernelName:
							path = kernelPath
							blockSizeDevice = kernelBlockSizeDevice

						case config.MemoryName:
							path = memoryPath
							blockSizeDevice = memoryBlockSizeDevice

						case config.StateName:
							path = statePath
							blockSizeDevice = stateBlockSizeDevice
						}

						if strings.TrimSpace(path) == "" {
							panic(ErrUnknownDeviceName)
						}

						receivedDevicesLock.Lock()
						receivedDevices = append(receivedDevices, di.Name)
						receivedDevicesLock.Unlock()

						receivedButNotReadyDevices.Add(1)

						if hook := hooks.OnDeviceReceived; hook != nil {
							hook(index, di.Name)
						}

						if err := os.MkdirAll(filepath.Dir(path), os.ModePerm); err != nil {
							panic(err)
						}

						storage, err := sources.NewFileStorageCreate(path, int64(di.Size))
						if err != nil {
							panic(err)
						}

						var remote *waitingcache.WaitingCacheRemote
						local, remote = waitingcache.NewWaitingCache(storage, int(di.Block_size))
						local.NeedAt = func(offset int64, length int32) {
							// Only access the `from` protocol if it's not already closed
							select {
							case <-protocolCtx.Done():
								return

							default:
							}

							if err := from.NeedAt(offset, length); err != nil {
								panic(err)
							}
						}
						local.DontNeedAt = func(offset int64, length int32) {
							// Only access the `from` protocol if it's not already closed
							select {
							case <-protocolCtx.Done():
								return

							default:
							}

							if err := from.DontNeedAt(offset, length); err != nil {
								panic(err)
							}
						}

						device := expose.NewExposedStorageNBDNL(local, 1, 0, local.Size(), blockSizeDevice, true)

						if err := device.Init(); err != nil {
							panic(err)
						}

						deviceCloseFuncsLock.Lock()
						deviceCloseFuncs = append(deviceCloseFuncs, device.Close)    // defer device.Close()
						deviceCloseFuncs = append(deviceCloseFuncs, device.Shutdown) // defer device.Shutdown()
						deviceCloseFuncsLock.Unlock()

						devicePath := filepath.Join("/dev", device.Device())

						deviceInfo, err := os.Stat(devicePath)
						if err != nil {
							panic(err)
						}

						deviceStat, ok := deviceInfo.Sys().(*syscall.Stat_t)
						if !ok {
							panic(ErrCouldNotGetNBDDeviceStat)
						}

						deviceMajor := uint64(deviceStat.Rdev / 256)
						deviceMinor := uint64(deviceStat.Rdev % 256)

						deviceID := int((deviceMajor << 8) | deviceMinor)

						if err := unix.Mknod(filepath.Join(runner.VMPath, di.Name), unix.S_IFBLK|0666, deviceID); err != nil {
							panic(err)
						}

						if hook := hooks.OnDeviceExposed; hook != nil {
							hook(index, devicePath)
						}

						return remote
					},
					p,
				)

				handleGoroutinePanics(true, func() {
					if err := from.HandleReadAt(); err != nil {
						panic(err)
					}
				})

				handleGoroutinePanics(true, func() {
					if err := from.HandleWriteAt(); err != nil {
						panic(err)
					}
				})

				handleGoroutinePanics(true, func() {
					if err := from.HandleDevInfo(); err != nil {
						panic(err)
					}
				})

				handleGoroutinePanics(true, func() {
					if err := from.HandleEvent(func(e *packets.Event) {
						switch e.Type {
						case packets.EventCustom:
							switch e.CustomType {
							case byte(EventCustomAllDevicesSent):
								cancelAllDevicesReceivedCtx()

								if hook := hooks.OnAllDevicesReceived; hook != nil {
									hook()
								}

							case byte(EventCustomTransferAuthority):
								if receivedButNotReadyDevices.Add(-1) <= 0 {
									cancelAllDevicesReadyCtx()
								}

								if hook := hooks.OnDeviceAuthorityReceived; hook != nil {
									hook(index)
								}
							}

						case packets.EventCompleted:
							if hook := hooks.OnDeviceMigrationCompleted; hook != nil {
								hook(index)
							}
						}
					}); err != nil {
						panic(err)
					}
				})

				handleGoroutinePanics(true, func() {
					if err := from.HandleDirtyList(func(blocks []uint) {
						if local != nil {
							local.DirtyBlocks(blocks)
						}
					}); err != nil {
						panic(err)
					}
				})
			})

		migratedPeer.Wait = sync.OnceValue(func() error {
			defer cancelProtocolCtx()

			if err := pro.Handle(); err != nil && !errors.Is(err, io.EOF) {
				return err
			}

			if hook := hooks.OnAllMigrationsCompleted; hook != nil {
				hook()
			}

			return nil
		})
		migratedPeer.Close = func() (errs error) {
			// We have to close the runner before we close the devices
			if err := runner.Close(); err != nil {
				errs = errors.Join(errs, err)
			}

			defer func() {
				if err := migratedPeer.Wait(); err != nil {
					errs = errors.Join(errs, err)
				}
			}()

			deviceCloseFuncsLock.Lock()
			defer deviceCloseFuncsLock.Unlock()

			for _, closeFunc := range deviceCloseFuncs {
				defer func(closeFunc func() error) {
					if err := closeFunc(); err != nil {
						errs = errors.Join(errs, err)
					}
				}(closeFunc)
			}

			return
		}

		// We don't track this because we return the wait function
		handleGoroutinePanics(false, func() {
			if err := migratedPeer.Wait(); err != nil {
				panic(err)
			}
		})

		// We don't track this because we return the close function
		handleGoroutinePanics(false, func() {
			select {
			// Failure case; we cancelled the internal context before all devices are ready
			case <-internalCtx.Done():
				if err := migratedPeer.Close(); err != nil {
					panic(err)
				}

			// Happy case; all devices are ready and we want to wait with closing the devices until we stop the Firecracker process
			case <-allDevicesReadyCtx.Done():
				<-hypervisorCtx.Done()

				if err := migratedPeer.Close(); err != nil {
					panic(err)
				}

				break
			}
		})

		select {
		case <-internalCtx.Done():
			panic(internalCtx.Err())
		case <-allDevicesReceivedCtx.Done():
			break
		}

		stage1Inputs := []peerStage1{
			{
				name:      config.StateName,
				base:      statePath,
				blockSize: stateBlockSizeStorage,
			},
			{
				name:      config.MemoryName,
				base:      memoryPath,
				blockSize: memoryBlockSizeStorage,
			},
			{
				name:      config.InitramfsName,
				base:      initramfsPath,
				blockSize: initramfsBlockSizeStorage,
			},
			{
				name:      config.KernelName,
				base:      kernelPath,
				blockSize: kernelBlockSizeStorage,
			},
			{
				name:      config.DiskName,
				base:      diskPath,
				blockSize: diskBlockSizeStorage,
			},
			{
				name:      config.ConfigName,
				base:      configPath,
				blockSize: configBlockSizeStorage,
			},
		}

		_, deferFuncs, err := iutils.ConcurrentMap(
			stage1Inputs,
			func(index int, input peerStage1, _ *struct{}, addDefer func(deferFunc func() error)) error {
				if slices.ContainsFunc(
					receivedDevices,
					func(r string) bool {
						return input.name == r
					},
				) {
					return nil
				}

				// TODO: Set up locally-backed device

				return nil
			},
		)

		// Make sure that we schedule the `deferFuncs` even if we get an error during device setup
		for _, deferFuncs := range deferFuncs {
			for _, deferFunc := range deferFuncs {
				deviceCloseFuncsLock.Lock()
				deviceCloseFuncs = append(deviceCloseFuncs, deferFunc) // defer deferFunc()
				deviceCloseFuncsLock.Unlock()
			}
		}

		if err != nil {
			panic(err)
		}

		select {
		case <-internalCtx.Done():
			panic(internalCtx.Err())
		case <-allDevicesReadyCtx.Done():
			break
		}

		migratedPeer.Resume = func(ctx context.Context, resumeTimeout time.Duration) (resumedPeer *ResumedRunner, errs error) {
			packageConfigFile, err := os.Open(configPath)
			if err != nil {
				return nil, err
			}
			defer packageConfigFile.Close()

			var packageConfig config.PackageConfiguration
			if err := json.NewDecoder(packageConfigFile).Decode(&packageConfig); err != nil {
				return nil, err
			}

			return runner.Resume(ctx, resumeTimeout, packageConfig.AgentVSockPort)
		}

		return
	}

	return
}
