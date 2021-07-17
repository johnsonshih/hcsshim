package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	vmOptions := hcsshim.VirtualMachineOptions{
		Name:               "static-ip-testVm",
		Id:                 "797BB510-E665-11EB-B250-A4BB6D44E185",
		VhdPath:            "c:\\temp\\AzureIoTEdgeForLinux-v1-EFLOW.vhdx",
		IsoPath:            "c:\\temp\\seed-static.iso",
		Owner:              "WssdTest",
		MemoryInMB:         1024,
		ProcessorCount:     1,
		VnicId:             "5258b965-465c-4ab3-928f-231d0bdaa2cd",
		MacAddress:         "00-15-5D-35-E9-A4",
		UseGuestConnection: true,
	}

	vmspec, err := CreateVirtualMachineSpec(&vmOptions)
	if err != nil {
		log.Printf("CreateVirtualMachineSpec failed: err = %v", err)
		return
	}

	hcsDocumentB, err := json.Marshal(vmspec.Spec)
	if err != nil {
		log.Printf("json.Marshal failed: err = %v", err)
		return
	}

	hcsDocument := string(hcsDocumentB)
	log.Printf("vmspec=%v", hcsDocument)

	system, err := hcs.CreateComputeSystem(ctx, "797BB510-E665-11EB-B250-A4BB6D44E185", vmspec.Spec)
	if err != nil {
		log.Printf("hcs.CreateComputeSystem failed: err = %v", err)
		return
	}
	defer system.Close()

	if err = system.Start(ctx); err != nil {
		log.Printf("system.Start failed: err = %v", err)
		return
	}

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
