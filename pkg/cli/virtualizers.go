package cli

/**
 * SPDX-License-Identifier: Apache-2.0
 * Copyright 2020 vorteil.io Pty Ltd
 */

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"

	"github.com/beeker1121/goque"
	"github.com/thanhpk/randstr"
	"github.com/vorteil/vorteil/pkg/ext"
	"github.com/vorteil/vorteil/pkg/vcfg"
	"github.com/vorteil/vorteil/pkg/vdisk"
	"github.com/vorteil/vorteil/pkg/vimg"
	"github.com/vorteil/vorteil/pkg/virtualizers/firecracker"
	"github.com/vorteil/vorteil/pkg/virtualizers/hyperv"
	"github.com/vorteil/vorteil/pkg/virtualizers/iputil"
	"github.com/vorteil/vorteil/pkg/virtualizers/qemu"
	"github.com/vorteil/vorteil/pkg/virtualizers/virtualbox"
	"github.com/vorteil/vorteil/pkg/virtualizers/vmware"
	"github.com/vorteil/vorteil/pkg/vpkg"
)

var ips *goque.Queue

// saveDisk attempts to moves disk from sourceDisk to destDisk
func saveDisk(sourceDisk, destDisk string) error {
	p := log.NewProgress("Copying Disk to "+destDisk, "", 0)
	err := os.Rename(sourceDisk, destDisk)
	if err != nil {
		log.Errorf("Failed to Copy Disk to '%s' error: %v\f", destDisk, err)
		p.Finish(false)
		return err
	}

	p.Finish(true)
	log.Printf("Copied Disk")
	return nil
}

// buildFirecracker does the same thing as vdisk.Build but it returns me a calver of the kernel being used
func buildFirecracker(ctx context.Context, w io.WriteSeeker, cfg *vcfg.VCFG, args *vdisk.BuildArgs) (string, error) {
	var err error
	for i := range cfg.Networks {
		if ips == nil {
			ips, err = iputil.NewIPStack()
			if err != nil {
				return "", err
			}
			defer ips.Close()

		}
		ip, err := ips.Dequeue()
		if err != nil {
			return "", err
		}
		cfg.Networks[i].IP = ip.ToString()
		cfg.Networks[i].Gateway = iputil.BridgeIP
		cfg.Networks[i].Mask = "255.255.255.0"
	}
	vimgBuilder, err := vdisk.CreateBuilder(ctx, &vimg.BuilderArgs{
		Kernel: vimg.KernelOptions{
			Shell: args.KernelOptions.Shell,
		},
		FSCompiler: ext.NewCompiler(&ext.CompilerArgs{
			FileTree: args.PackageReader.FS(),
			Logger:   args.Logger,
		}),
		VCFG:   cfg,
		Logger: log,
	})
	if err != nil {
		return "", err
	}
	defer vimgBuilder.Close()
	vimgBuilder.SetDefaultMTU(args.Format.DefaultMTU())
	err = vdisk.NegotiateSize(ctx, vimgBuilder, cfg, args)
	if err != nil {
		return "", err
	}

	err = args.Format.Build(ctx, log, w, vimgBuilder, cfg)
	if err != nil {
		return "", err
	}
	return string(vimgBuilder.KernelUsed()), nil
}

// runVMware
//	Saves resulting image to diskOutput if it's not an empty string
func runVMware(pkgReader vpkg.Reader, cfg *vcfg.VCFG, name, diskOutput string) error {
	if !vmware.Allocator.IsAvailable() {
		return errors.New("vmware is not installed on your system")
	}

	var err error
	// Create base folder to store vmware vms so the socket can be grouped
	parent := fmt.Sprintf("%s-%s", vmware.VirtualizerID, randstr.Hex(5))
	parent = filepath.Join(os.TempDir(), parent)

	// Create parent directory as it doesn't exist
	err = os.MkdirAll(parent, os.ModePerm)
	if err != nil {
		return err
	}

	// need to create a tempfile rather than use the function to as vmware complains if the extension doesn't exist
	f, err := os.Create(filepath.Join(parent, "disk.vmdk"))
	if err != nil {
		return err
	}

	defer func() {
		f.Close()
		// Move disk to diskOutput
		if diskOutput != "" {
			saveDisk(f.Name(), diskOutput)
		}
		os.Remove(f.Name())
		os.Remove(parent)

	}()

	err = vdisk.Build(context.Background(), f, &vdisk.BuildArgs{
		WithVCFGDefaults: true,
		PackageReader:    pkgReader,
		Format:           vmware.Allocator.DiskFormat(),
		KernelOptions: vdisk.KernelOptions{
			Shell:  flagShell,
			Record: flagRecord != "",
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	err = pkgReader.Close()
	if err != nil {
		return err
	}

	alloc := vmware.Allocator
	virt := alloc.Alloc()

	config := vmware.Config{
		Headless:    !flagGUI,
		NetworkType: "nat",
	}

	err = virt.Initialize(config.Marshal())
	if err != nil {
		return err
	}

	err = vcfg.WithDefaults(cfg, log)
	if err != nil {
		return err
	}

	return run(virt, f.Name(), cfg, name)
}

// runFirecracker needs a longer build process so we can pull the calver of the kernel used to build the disk
//	Saves resulting image to diskOutput if it's not an empty string
func runFirecracker(pkgReader vpkg.Reader, cfg *vcfg.VCFG, name, diskOutput string) error {
	var err error
	if runtime.GOOS != "linux" {
		return errors.New("firecracker is only available on linux")
	}
	if !firecracker.Allocator.IsAvailable() {
		return errors.New("firecracker is not installed on your system")
	}

	err = firecracker.FetchBridgeDevice()
	if err != nil {
		// Set bridge device to 10.26.10.1
		err = firecracker.SetupBridge(log, iputil.BridgeIP)
		if err != nil {
			return err
		}
	}

	// Create base folder to store firecracker vms so the socket can be grouped
	parent := fmt.Sprintf("%s-%s", firecracker.VirtualizerID, randstr.Hex(5))
	parent = filepath.Join(os.TempDir(), parent)
	defer os.RemoveAll(parent)

	// Create parent directory as it doesn't exist
	err = os.MkdirAll(parent, os.ModePerm)
	if err != nil {
		return err
	}

	f, err := ioutil.TempFile(parent, "vorteil.disk")
	if err != nil {
		return err
	}

	defer func() {
		f.Close()
		// Move disk to diskOutput
		if diskOutput != "" {
			saveDisk(f.Name(), diskOutput)
		}
		os.Remove(f.Name())
		os.Remove(parent)

	}()

	err = vcfg.WithDefaults(cfg, log)
	if err != nil {
		return err
	}

	kernelVer, err := buildFirecracker(context.Background(), f, cfg, &vdisk.BuildArgs{
		WithVCFGDefaults: true,
		PackageReader:    pkgReader,
		Format:           firecracker.Allocator.DiskFormat(),
		KernelOptions: vdisk.KernelOptions{
			Shell:  flagShell,
			Record: flagRecord != "",
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	// assign kernel version that was built with vcfg
	cfg.VM.Kernel = kernelVer

	err = f.Close()
	if err != nil {
		return err
	}

	err = pkgReader.Close()
	if err != nil {
		return err
	}

	alloc := firecracker.Allocator
	virt := alloc.Alloc()

	if flagGUI {
		log.Warnf("firecracker does not support displaying a gui")
	}

	config := firecracker.Config{}

	err = virt.Initialize(config.Marshal())
	if err != nil {
		return err
	}

	return run(virt, f.Name(), cfg, name)
}

// runHyperV
//	Saves resulting image to diskOutput if it's not an empty string
func runHyperV(pkgReader vpkg.Reader, cfg *vcfg.VCFG, name, diskOutput string) error {
	if runtime.GOOS != "windows" {
		return errors.New("hyper-v is only available on windows system")
	}
	if !hyperv.Allocator.IsAvailable() {
		return errors.New("hyper-v is not enabled on your system")
	}
	// Create base folder to store hyper-v vms so the socket can be grouped
	parent := fmt.Sprintf("%s-%s", hyperv.VirtualizerID, randstr.Hex(5))
	parent = filepath.Join(os.TempDir(), parent)

	// Create parent directory as it doesn't exist
	err := os.MkdirAll(parent, os.ModePerm)
	if err != nil {
		return err
	}

	// need to create a tempfile rather than use the function to as hyper-v complains if the extension doesn't exist
	f, err := os.Create(filepath.Join(parent, "disk.vhd"))
	if err != nil {
		return err
	}

	defer func() {
		f.Close()
		// Move disk to diskOutput
		if diskOutput != "" {
			saveDisk(f.Name(), diskOutput)
		}
		os.Remove(f.Name())
		os.Remove(parent)

	}()

	err = vdisk.Build(context.Background(), f, &vdisk.BuildArgs{
		WithVCFGDefaults: true,
		PackageReader:    pkgReader,
		Format:           hyperv.Allocator.DiskFormat(),
		KernelOptions: vdisk.KernelOptions{
			Shell:  flagShell,
			Record: flagRecord != "",
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	err = pkgReader.Close()
	if err != nil {
		return err
	}

	alloc := hyperv.Allocator
	virt := alloc.Alloc()

	config := hyperv.Config{
		Headless:   !flagGUI,
		SwitchName: "Default Switch",
	}

	err = virt.Initialize(config.Marshal())
	if err != nil {
		return err
	}

	err = vcfg.WithDefaults(cfg, log)
	if err != nil {
		return err
	}

	return run(virt, f.Name(), cfg, name)
}

// runVirtualBox
//	Saves resulting image to diskOutput if it's not an empty string
func runVirtualBox(pkgReader vpkg.Reader, cfg *vcfg.VCFG, name, diskOutput string) error {
	if !virtualbox.Allocator.IsAvailable() {
		return errors.New("virtualbox not found installed on system")
	}
	// Create base folder to store virtualbox vms so the socket can be grouped
	parent := fmt.Sprintf("%s-%s", virtualbox.VirtualizerID, randstr.Hex(5))
	parent = filepath.Join(os.TempDir(), parent)
	defer os.RemoveAll(parent)

	// Create parent directory as it doesn't exist
	err := os.MkdirAll(parent, os.ModePerm)
	if err != nil {
		return err
	}

	f, err := ioutil.TempFile(parent, "vorteil.disk")
	if err != nil {
		return err
	}

	defer func() {
		f.Close()
		// Move disk to diskOutput
		if diskOutput != "" {
			saveDisk(f.Name(), diskOutput)
		}
		os.Remove(f.Name())
		os.Remove(parent)

	}()

	err = vdisk.Build(context.Background(), f, &vdisk.BuildArgs{
		WithVCFGDefaults: true,
		PackageReader:    pkgReader,
		Format:           virtualbox.Allocator.DiskFormat(),
		KernelOptions: vdisk.KernelOptions{
			Shell:  flagShell,
			Record: flagRecord != "",
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	err = pkgReader.Close()
	if err != nil {
		return err
	}

	alloc := virtualbox.Allocator
	virt := alloc.Alloc()

	config := virtualbox.Config{
		Headless:    !flagGUI,
		NetworkType: "nat",
	}

	err = virt.Initialize(config.Marshal())
	if err != nil {
		return err
	}

	err = vcfg.WithDefaults(cfg, log)
	if err != nil {
		return err
	}

	return run(virt, f.Name(), cfg, name)
}

// runQEMU
//	Saves resulting image to diskOutput if it's not an empty string
func runQEMU(pkgReader vpkg.Reader, cfg *vcfg.VCFG, name string, diskOutput string) error {

	if !qemu.Allocator.IsAvailable() {
		return errors.New("qemu not installed on system")
	}
	// Create base folder to store qemu vms so the socket can be grouped
	parent := fmt.Sprintf("%s-%s", qemu.VirtualizerID, randstr.Hex(5))
	parent = filepath.Join(os.TempDir(), parent)
	defer os.RemoveAll(parent)
	// Create parent directory as it doesn't exist
	err := os.MkdirAll(parent, os.ModePerm)
	if err != nil {
		return err
	}
	f, err := ioutil.TempFile(parent, "vorteil.disk")
	if err != nil {
		return err
	}

	defer func() {
		f.Close()
		// Move disk to diskOutput
		if diskOutput != "" {
			saveDisk(f.Name(), diskOutput)
		}
		os.Remove(f.Name())
		os.Remove(parent)

	}()

	err = vdisk.Build(context.Background(), f, &vdisk.BuildArgs{
		WithVCFGDefaults: true,
		PackageReader:    pkgReader,
		Format:           qemu.Allocator.DiskFormat(),
		KernelOptions: vdisk.KernelOptions{
			Shell:  flagShell,
			Record: flagRecord != "",
		},
		Logger: log,
	})
	if err != nil {
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	err = pkgReader.Close()
	if err != nil {
		return err
	}

	alloc := qemu.Allocator
	virt := alloc.Alloc()

	config := qemu.Config{
		Headless: !flagGUI,
	}

	err = virt.Initialize(config.Marshal())
	if err != nil {
		return err
	}

	err = vcfg.WithDefaults(cfg, log)
	if err != nil {
		return err
	}

	return run(virt, f.Name(), cfg, name)
}
