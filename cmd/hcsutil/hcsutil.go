package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
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

func CreateVirtualMachineSpec(name, id, vhdPath, isoPath, owner string, memoryInMB, processorCount int, vnicId, macAddress string) (*VirtualMachineSpec, error) {
	spec := &hcsschema.ComputeSystem{
		Owner: owner,
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
					SizeInMB: int32(memoryInMB),
				},
				Processor: &hcsschema.Processor2{
					Count: int32(processorCount),
				},
			},
			Devices: &hcsschema.Devices{
				Scsi: map[string]hcsschema.Scsi{
					"primary": hcsschema.Scsi{
						Attachments: map[string]hcsschema.Attachment{
							"0": hcsschema.Attachment{
								Path:  vhdPath,
								Type_: "VirtualDisk",
							},
							"1": hcsschema.Attachment{
								Path:  isoPath,
								Type_: "Iso",
							},
						},
					},
				},
				NetworkAdapters: map[string]hcsschema.NetworkAdapter{},
			},
			// GuestConnection: &hcsschema.GuestConnection{
			//	UseVsock:            true,
			//	UseConnectedSuspend: true,
			//},
		},
	}

	if len(vnicId) > 0 {
		spec.VirtualMachine.Devices.NetworkAdapters["ext"] = hcsschema.NetworkAdapter{
			EndpointId: vnicId,
			MacAddress: macAddress,
		}
	}

	return &VirtualMachineSpec{
		Spec: spec,
		ID:   id,
		Name: name,
	}, nil
}

func main() {
	vmspec, err := CreateVirtualMachineSpec("vmname", "vm-id", "c:\\temp\\AzureIoTEdgeForLinux-v1-EFLOW.vhdx", "c:\\temp\\seed-static.iso", "vmowner", 1024, 1, "JSHIH-DELL2-EFLOWInterface", "vmmacAddress")
	if err != nil {
		log.Printf("CreateVirtualMachineSpec failed: err = %v", err)
	}
	log.Printf("vmspec=%v", vmspec)
}

func modifyComputeSystem(vmName string, jsonFileName string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	requestContent, err := ioutil.ReadFile(jsonFileName)
	if err != nil {
		log.Printf("ReadFile json %s failed, err = %v", jsonFileName, err)
		return
	}

	var request hcsschema.ModifySettingRequest

	err = json.Unmarshal(requestContent, &request)
	if err != nil {
		log.Printf("Unmarshal json failed, err = %v", err)
		return
	}

	system, err := hcs.OpenComputeSystem(ctx, vmName)
	if err != nil {
		log.Printf("OpenComputeSystem failed, err = %v", err)
		return
	}
	defer system.Close()

	if err = system.Modify(ctx, request); err != nil {
		log.Printf("ModifyComputeSystem failed, err = %v", err)
		return
	}

	log.Printf("Waiting for ModifyComputeSystem complete...")
	system.Wait()

	log.Printf("ModifyComputeSystem Succeeded")
	return
}
