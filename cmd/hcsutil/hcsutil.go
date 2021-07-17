package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"time"

	"github.com/Microsoft/hcsshim"
	"github.com/Microsoft/hcsshim/internal/hcs"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/Microsoft/hcsshim/internal/wclayer"
)

var (
	vmNameFlag       = flag.String("vmname", "", "vm name")
	jsonFileNameFlag = flag.String("json", "", "modify request json path")
)

type VirtualMachineSpec struct {
	Name      string
	ID        string
	runtimeId string
	Spec      *hcsschema.ComputeSystem
	system    *hcs.System
}

func CreateVirtualMachineSpec(opts *hcsshim.VirtualMachineOptions) (*VirtualMachineSpec, error) {
	// Ensure the VM has access, we use opts.Id to create VM
	if err := wclayer.GrantVmAccess(opts.Id, opts.VhdPath); err != nil {
		return nil, err
	}
	if err := wclayer.GrantVmAccess(opts.Id, opts.IsoPath); err != nil {
		return nil, err
	}

	spec := &hcsschema.ComputeSystem{
		Owner: opts.Owner,
		SchemaVersion: &hcsschema.Version{
			Major: 2,
			Minor: 1,
		},
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: &hcsschema.VirtualMachine{
			Chipset: &hcsschema.Chipset{
				Uefi: &hcsschema.Uefi{
					BootThis: &hcsschema.UefiBootEntry{
						DevicePath: "primary",
						DeviceType: "ScsiDrive",
						//OptionalData: "ds=nocloud;h=lmasterm;i=test;s=/opt/cloud/metadata",
					},
				},
			},
			ComputeTopology: &hcsschema.Topology{
				Memory: &hcsschema.Memory2{
					SizeInMB:        int32(opts.MemoryInMB),
					AllowOvercommit: opts.AllowOvercommit,
				},
				Processor: &hcsschema.Processor2{
					Count: int32(opts.ProcessorCount),
				},
			},
			Devices: &hcsschema.Devices{
				Scsi: map[string]hcsschema.Scsi{
					"primary": {
						Attachments: map[string]hcsschema.Attachment{
							"0": {
								Path:  opts.VhdPath,
								Type_: "VirtualDisk",
							},
							"1": {
								Path:  opts.IsoPath,
								Type_: "Iso",
							},
						},
					},
				},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{},
				Plan9:           &hcsschema.Plan9{},
			},
		},
	}

	if len(opts.VnicId) > 0 {
		spec.VirtualMachine.Devices.NetworkAdapters["ext"] = hcsschema.NetworkAdapter{
			EndpointId: opts.VnicId,
			MacAddress: opts.MacAddress,
		}
	}

	if opts.UseGuestConnection {
		spec.VirtualMachine.GuestConnection = &hcsschema.GuestConnection{
			UseVsock:            true,
			UseConnectedSuspend: true,
		}
	}

	return &VirtualMachineSpec{
		Spec: spec,
		ID:   opts.Id,
		Name: opts.Name,
	}, nil
}

func main() {
	flag.Parse()
	if flag.NArg() != 0 || len(*vmNameFlag) == 0 || len(*jsonFileNameFlag) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	system, err := bootComputeSystem(*vmNameFlag, *jsonFileNameFlag)
	if err != nil {
		os.Exit(1)
	}
	defer system.Close()

	fmt.Printf("Press any key to stop!!!")
	fmt.Scanln()
	os.Exit(0)
}

func bootComputeSystem(vmName string, jsonFileName string) (_ *hcs.System, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	requestContent, err := ioutil.ReadFile(jsonFileName)
	if err != nil {
		log.Printf("ReadFile json %s failed, err = %v", jsonFileName, err)
		return
	}

	var request hcsschema.ComputeSystem

	err = json.Unmarshal(requestContent, &request)
	if err != nil {
		log.Printf("Unmarshal json failed, err = %v", err)
		return
	}

	system, err := hcs.CreateComputeSystem(ctx, vmName, request)
	if err != nil {
		log.Printf("hcs.CreateComputeSystem failed: err = %v", err)
		return
	}

	if err = system.Start(ctx); err != nil {
		log.Printf("system.Start failed: err = %v", err)
		system.Close()
		return
	}

	log.Printf("bootComputeSystem Succeeded")
	return system, err
}
