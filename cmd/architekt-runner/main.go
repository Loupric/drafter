package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"time"

	"github.com/loopholelabs/architekt/pkg/config"
	"github.com/loopholelabs/architekt/pkg/firecracker"
	"github.com/loopholelabs/architekt/pkg/mount"
	"github.com/loopholelabs/architekt/pkg/roles"
	"github.com/loopholelabs/architekt/pkg/utils"
	"golang.org/x/sys/unix"
)

func main() {
	rawFirecrackerBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary")
	rawJailerBin := flag.String("jailer-bin", "jailer", "Jailer binary (from Firecracker)")

	chrootBaseDir := flag.String("chroot-base-dir", filepath.Join("out", "vms"), "`chroot` base directory")
	cacheBaseDir := flag.String("cache-base-dir", filepath.Join("out", "cache"), "Cache base directory")

	uid := flag.Int("uid", 0, "User ID for the Firecracker process")
	gid := flag.Int("gid", 0, "Group ID for the Firecracker process")

	enableOutput := flag.Bool("enable-output", true, "Whether to enable VM stdout and stderr")
	enableInput := flag.Bool("enable-input", false, "Whether to enable VM stdin")

	resumeTimeout := flag.Duration("resume-timeout", time.Minute, "Maximum amount of time to wait for agent to resume")

	netns := flag.String("netns", "ark0", "Network namespace to run Firecracker in")

	numaNode := flag.Int("numa-node", 0, "NUMA node to run Firecracker in")
	cgroupVersion := flag.Int("cgroup-version", 2, "Cgroup version to use for Jailer")

	packagePath := flag.String("package-path", filepath.Join("out", "redis.ark"), "Path to package to use")

	persist := flag.Bool("persist", true, "Whether to write back changes after stopping the VM")

	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	firecrackerBin, err := exec.LookPath(*rawFirecrackerBin)
	if err != nil {
		panic(err)
	}

	jailerBin, err := exec.LookPath(*rawJailerBin)
	if err != nil {
		panic(err)
	}

	packageFile, err := os.OpenFile(*packagePath, os.O_RDWR, os.ModePerm)
	if err != nil {
		panic(err)
	}
	defer packageFile.Close()

	packageArchive := tar.NewReader(packageFile)

	packageConfig, packageConfigInfo, err := utils.ReadPackageConfigFromTar(packageArchive)
	if err != nil {
		panic(err)
	}

	if _, err := packageFile.Seek(0, io.SeekStart); err != nil {
		panic(err)
	}

	packageArchive = tar.NewReader(packageFile)

	if err := os.MkdirAll(*cacheBaseDir, os.ModePerm); err != nil {
		panic(err)
	}

	cacheDir, err := os.MkdirTemp(*cacheBaseDir, "")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(cacheDir)

	for {
		header, err := packageArchive.Next()
		if err != nil {
			if err == io.EOF {
				break
			}

			panic(err)
		}

		if err != nil {
			panic(err)
		}

		target := filepath.Join(cacheDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				panic(err)
			}

		case tar.TypeReg:
			f, err := os.Create(target)
			if err != nil {
				panic(err)
			}

			if _, err := io.Copy(f, packageArchive); err != nil {
				_ = f.Close()

				panic(err)
			}

			_ = f.Close()
		}
	}

	runner := roles.NewRunner(
		config.HypervisorConfiguration{
			FirecrackerBin: firecrackerBin,
			JailerBin:      jailerBin,

			ChrootBaseDir: *chrootBaseDir,

			UID: *uid,
			GID: *gid,

			NetNS:         *netns,
			NumaNode:      *numaNode,
			CgroupVersion: *cgroupVersion,

			EnableOutput: *enableOutput,
			EnableInput:  *enableInput,
		},
		config.AgentConfiguration{
			AgentVSockPort: packageConfig.AgentVSockPort,
			ResumeTimeout:  *resumeTimeout,
		},
	)

	var wg sync.WaitGroup
	defer wg.Wait()

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := runner.Wait(); err != nil {
			panic(err)
		}
	}()

	defer runner.Close()
	vmPath, err := runner.Open()
	if err != nil {
		panic(err)
	}

	for _, file := range []string{
		firecracker.StateName,
		firecracker.MemoryName,
		roles.InitramfsName,
		roles.KernelName,
		roles.DiskName,
	} {
		mnt := mount.NewLoopMount(filepath.Join(cacheDir, file))

		dev, err := mnt.Open()
		if err != nil {
			panic(err)
		}
		defer mnt.Close()

		if err := unix.Mknod(filepath.Join(vmPath, firecracker.MountName, file), unix.S_IFBLK|0666, dev); err != nil {
			panic(err)
		}
	}

	before := time.Now()

	if err := runner.Resume(ctx); err != nil {
		panic(err)
	}

	log.Println("Resume:", time.Since(before))

	if *persist {
		defer func() {
			if err := packageFile.Truncate(0); err != nil {
				panic(err)
			}

			if _, err := packageFile.Seek(0, io.SeekStart); err != nil {
				panic(err)
			}

			packageOutputArchive := tar.NewWriter(packageFile)
			defer packageOutputArchive.Close()

			for _, file := range []string{
				firecracker.StateName,
				firecracker.MemoryName,
				roles.InitramfsName,
				roles.KernelName,
				roles.DiskName,
			} {
				info, err := os.Stat(filepath.Join(cacheDir, file))
				if err != nil {
					panic(err)
				}

				header, err := tar.FileInfoHeader(info, filepath.Join(cacheDir, file))
				if err != nil {
					panic(err)
				}
				header.Name = file

				if err := packageOutputArchive.WriteHeader(header); err != nil {
					panic(err)
				}

				f, err := os.Open(filepath.Join(cacheDir, file))
				if err != nil {
					panic(err)
				}
				defer f.Close()

				if _, err = io.Copy(packageOutputArchive, f); err != nil {
					panic(err)
				}
			}

			header, err := tar.FileInfoHeader(packageConfigInfo, filepath.Join(cacheDir, utils.PackageConfigName))
			if err != nil {
				panic(err)
			}
			header.Name = utils.PackageConfigName

			if err := packageOutputArchive.WriteHeader(header); err != nil {
				panic(err)
			}

			packageConfig, err := json.Marshal(utils.PackageConfig{
				AgentVSockPort: packageConfig.AgentVSockPort,
			})
			if err != nil {
				panic(err)
			}

			if _, err := packageOutputArchive.Write(packageConfig); err != nil {
				panic(err)
			}
		}()
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt)

	<-done

	before = time.Now()

	if err := runner.Suspend(ctx); err != nil {
		panic(err)
	}

	log.Println("Suspend:", time.Since(before))
}
