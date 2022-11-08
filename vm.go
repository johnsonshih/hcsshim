package hcsshim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"path"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/Microsoft/go-winio/pkg/guid"
	"github.com/Microsoft/hcsshim/hcn"
	"github.com/Microsoft/hcsshim/internal/gcs"
	"github.com/Microsoft/hcsshim/internal/guestrequest"
	"github.com/Microsoft/hcsshim/internal/hcs"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/logfields"
	"github.com/Microsoft/hcsshim/internal/requesttype"
	"github.com/Microsoft/hcsshim/internal/schema1"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/Microsoft/hcsshim/internal/wclayer"
	"github.com/Microsoft/hcsshim/osversion"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/windows"
)

type GpuAssignmentMode string

const (
	GpuAssignmentModeDisabled = GpuAssignmentMode("Disabled")
	GpuAssignmentModeDefault  = GpuAssignmentMode("Default")
	GpuAssignmentModeList     = GpuAssignmentMode("List")
	GpuAssignmentModeMirror   = GpuAssignmentMode("Mirror")
)

type ShutdownMechanism string

const (
	ShutdownMechanismGuestConnection    = ShutdownMechanism("GuestConnection")
	ShutdownMechanismIntegrationService = ShutdownMechanism("IntegrationService")
)

type ShutdownType string

const (
	ShutdownTypeShutdown  = ShutdownType("Shutdown")
	ShutdownTypeHibernate = ShutdownType("Hibernate")
	ShutdownTypeReboot    = ShutdownType("Reboot")
)

type VirtualMachineOptions struct {
	Name                    string
	Id                      string
	VhdPath                 string
	IsoPath                 string
	Owner                   string
	MemoryInMB              int32
	ProcessorCount          int32
	VnicId                  string
	MacAddress              string
	UseGuestConnection      bool
	ExternalGuestConnection bool // sets whether the guest RPC connection is performed internally by the OS platform or externally by this package.
	GuestConnectionUseVsock bool
	AllowOvercommit         bool
	SecureBootEnabled       bool
	SecureBootTemplateId    string
	HighMmioBaseInMB        int32
	HighMmioGapInMB         int32
}

const plan9Port = 564

type VirtualMachineSpec struct {
	Name       string
	ID         string
	runtimeId  guid.GUID
	spec       *hcsschema.ComputeSystem
	system     *hcs.System
	extGcs     bool
	gcListener net.Listener         // The GCS connection listener
	gc         *gcs.GuestConnection // The GCS connection

	// GCS bridge protocol and capabilities
	protocol  uint32
	guestCaps schema1.GuestDefinedCapabilities
}

func CreateVirtualMachineSpec(opts *VirtualMachineOptions) (*VirtualMachineSpec, error) {
	// Ensure the VM has access, we use opts.Id to create VM
	if err := wclayer.GrantVmAccess(opts.Id, opts.VhdPath); err != nil {
		return nil, err
	}
	if err := wclayer.GrantVmAccess(opts.Id, opts.IsoPath); err != nil {
		return nil, err
	}

	// determine which schema version to use
	schemaVersion := getSchemaVersion(opts)

	spec := &hcsschema.ComputeSystem{
		Owner:                             opts.Owner,
		SchemaVersion:                     &schemaVersion,
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
		if !opts.ExternalGuestConnection {
			// gcs connects to hcs (internal)
			spec.VirtualMachine.GuestConnection = &hcsschema.GuestConnection{
				UseVsock:            opts.GuestConnectionUseVsock,
				UseConnectedSuspend: true,
			}
		} else {
			// gcs connects to hcsshim (external)
			// Allow administrators and SYSTEM to bind to vsock sockets
			// so that we can create GCS sockets.
			spec.VirtualMachine.Devices.HvSocket = &hcsschema.HvSocket2{
				HvSocketConfig: &hcsschema.HvSocketSystemConfig{
					DefaultBindSecurityDescriptor: "D:P(A;;FA;;;SY)(A;;FA;;;BA)",
				},
			}
		}
	}

	if opts.SecureBootEnabled {
		spec.VirtualMachine.Chipset.Uefi.SecureBootTemplateId = opts.SecureBootTemplateId
		spec.VirtualMachine.Chipset.Uefi.ApplySecureBootTemplate = "Apply"
	}

	if opts.HighMmioBaseInMB != 0 {
		spec.VirtualMachine.ComputeTopology.Memory.HighMmioBaseInMB = opts.HighMmioBaseInMB
	}

	if opts.HighMmioGapInMB != 0 {
		spec.VirtualMachine.ComputeTopology.Memory.HighMmioGapInMB = opts.HighMmioGapInMB
	}

	return &VirtualMachineSpec{
		spec:   spec,
		ID:     opts.Id,
		Name:   opts.Name,
		extGcs: opts.UseGuestConnection && opts.ExternalGuestConnection,
	}, nil
}

func getHcsSpec(system *hcs.System) *hcsschema.ComputeSystem {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	_, err := system.Properties(ctx)
	if err != nil {
		return nil
	}
	// FixMe - return proper Compute System schema
	return nil
}

func GetVirtualMachineState(id string) string {
	properties, err := GetVirtualMachineProperties(id)
	if err != nil {
		return ""
	}
	return properties.State
}

func GetVirtualMachineProperties(id string) (*schema1.ContainerProperties, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, id)
	if err != nil {
		return nil, err
	}
	defer system.Close()

	return system.Properties(ctx)
}

func GetVirtualMachineSpec(id string) (*VirtualMachineSpec, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, id)
	if err != nil {
		return nil, err
	}

	return &VirtualMachineSpec{
		ID:     id,
		system: system,
		spec:   getHcsSpec(system),
	}, nil

}

// HasVirtualMachine
func HasVirtualMachine(id string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, id)
	if err != nil {
		if hcs.IsNotExist(err) {
			return false
		} else {
			return true
		}
	}
	defer system.Close()

	return true
}

// List all/specified Virtual Machine
func ListVirtualMachines(id string) ([]*VirtualMachineSpec, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	query := ComputeSystemQuery{
		Types: []string{"VirtualMachine"},
	}
	if len(id) != 0 {
		query.IDs = []string{id}
	}

	vms := []*VirtualMachineSpec{}
	vmproperties, err := hcs.GetComputeSystems(ctx, query)
	if err != nil {
		return vms, err
	}

	for _, vmprop := range vmproperties {
		vm, err := GetVirtualMachineSpec(vmprop.ID)
		if err != nil {
			return vms, err
		}
		vms = append(vms, vm)

	}

	return vms, nil

}

// Create a Virtual Machine
func (vm *VirtualMachineSpec) Create() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.CreateComputeSystem(ctx, vm.ID, vm.spec)
	if err != nil {
		return err
	}
	properties, err := system.Properties(ctx)
	if err != nil {
		return err
	}

	vm.runtimeId = properties.RuntimeID
	vm.system = system
	if vm.extGcs {
		l, err := vm.listenVsock(gcs.LinuxGcsVsockPort)
		if err != nil {
			return err
		}
		vm.gcListener = l
	}

	return nil
}

// Start Virtual Machine
func (vm *VirtualMachineSpec) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	err = system.Start(ctx)
	if err != nil {
		return err
	}

	if vm.gcListener != nil {
		// Accept the GCS connection.
		conn, err := vm.acceptAndClose(ctx, vm.gcListener)
		vm.gcListener = nil
		if err != nil {
			return fmt.Errorf("failed to connect to GCS: %s", err)
		}

		// Start the GCS protocol.
		gcc := &gcs.GuestConnectionConfig{
			Conn:     conn,
			Log:      logrus.WithField(logfields.UVMID, vm.ID),
			IoListen: gcs.HvsockIoListen(vm.runtimeId),
		}
		vm.gc, err = gcc.Connect(ctx)
		if err != nil {
			return err
		}
		vm.guestCaps = *vm.gc.Capabilities()
		vm.protocol = vm.gc.Protocol()
	} else {
		// Get the guest connection properties from compute system
		properties, err := vm.system.Properties(ctx, schema1.PropertyTypeGuestConnection)
		if err != nil {
			return err
		}
		vm.guestCaps = properties.GuestConnectionInfo.GuestDefinedCapabilities
		vm.protocol = properties.GuestConnectionInfo.ProtocolVersion
	}

	return nil
}

// Stop a Virtual Machine
func (vm *VirtualMachineSpec) Stop(force bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	if vm.gc != nil {
		// external guest connection used, shutdown through the external gcs
		return vm.shutdownThroughGuestConnection(ctx, force)
	}

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	shutdownOptions, err := generateShutdownOptions(force)
	if err != nil {
		return err
	}

	return system.Shutdown(ctx, shutdownOptions)
}

// Delete a Virtual Machine
func (vm *VirtualMachineSpec) Delete() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		if hcs.IsNotExist(err) {
			return nil
		} else {
			return err
		}
	}
	defer system.Close()

	return system.Terminate(ctx)
}

// Wait for a Virtual Machine exits
func (vm *VirtualMachineSpec) Wait() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	return system.Wait()
}

// ExecuteCommand executes a command in the Virtual Machine
func (vm *VirtualMachineSpec) ExecuteCommand(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	return nil
}

// escapeArgs makes a Windows-style escaped command line from a set of arguments
func escapeArgs(args []string) string {
	escapedArgs := make([]string, len(args))
	for i, a := range args {
		escapedArgs[i] = windows.EscapeArg(a)
	}
	return strings.Join(escapedArgs, " ")
}

// RunCommand executes a command on the Virtual Machine
func (vm *VirtualMachineSpec) RunCommand(command []string, user string) (exitCode int, output string, errOut string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	exitCode = 1

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return exitCode, "", "", err
	}
	defer system.Close()

	var params *hcsschema.ProcessParameters
	switch system.OS() {
	case "linux":
		params = &hcsschema.ProcessParameters{
			CommandArgs:      command,
			WorkingDirectory: "/",
			User:             user,
			Environment:      map[string]string{"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			CreateStdInPipe:  false,
			CreateStdOutPipe: true,
			CreateStdErrPipe: true,
			ConsoleSize:      []int32{0, 0},
		}
	case "windows":
		params = &hcsschema.ProcessParameters{
			CommandLine:      escapeArgs(command),
			WorkingDirectory: `C:\`,
			User:             user,
			CreateStdInPipe:  false,
			CreateStdOutPipe: true,
			CreateStdErrPipe: true,
			ConsoleSize:      []int32{0, 0},
		}
	default:
		return exitCode, "", "", ErrNotSupported
	}

	process, err := system.CreateProcess(ctx, params)
	if err != nil {
		return
	}

	defer process.Close()

	err = process.Wait()
	if err != nil {
		return exitCode, "Wait returned error!", "", err
	}

	exitCode, err = process.ExitCode()

	_, reader, errReader := process.Stdio()
	if reader != nil {
		outBuf := new(bytes.Buffer)
		outBuf.ReadFrom(reader)
		output = strings.TrimSpace(outBuf.String())
	}

	if errReader != nil {
		errBuf := new(bytes.Buffer)
		errBuf.ReadFrom(errReader)
		errOut = strings.TrimSpace(errBuf.String())
	}

	return
}

func (vm *VirtualMachineSpec) HotAttachEndpoints(endpoints []*hcn.HostComputeEndpoint) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	for _, endpoint := range endpoints {
		if err = vm.hotAttachEndpoint(ctx, system, endpoint); err != nil {
			return err
		}
	}
	return nil
}

func (vm *VirtualMachineSpec) HotDetachEndpoint(endpoint *hcn.HostComputeEndpoint) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	// Hot detach an endpoint from the compute system
	request := &hcsschema.ModifySettingRequest{
		RequestType:  requesttype.Remove,
		ResourcePath: path.Join("VirtualMachine/Devices/NetworkAdapters", endpoint.Id),
		Settings: hcsschema.NetworkAdapter{
			EndpointId: endpoint.Id,
			MacAddress: endpoint.MacAddress,
		},
	}

	if err = vm.modifySetting(ctx, request); err != nil {
		return err
	}

	return nil
}

func (vm *VirtualMachineSpec) hotAttachEndpoint(ctx context.Context, system *hcs.System, endpoint *hcn.HostComputeEndpoint) (err error) {
	// Hot attach an endpoint to the compute system
	request := &hcsschema.ModifySettingRequest{
		RequestType:  requesttype.Add,
		ResourcePath: path.Join("VirtualMachine/Devices/NetworkAdapters", endpoint.Id),
		Settings: hcsschema.NetworkAdapter{
			EndpointId: endpoint.Id,
			MacAddress: endpoint.MacAddress,
		},
	}

	if err = vm.modifySetting(ctx, request); err != nil {
		return err
	}

	return nil
}

// AddPlan9 adds a Plan9 share to a VirtualMachineSpec.
func (vm *VirtualMachineSpec) AddPlan9(shareName string, hostPath string, uvmPath string, readOnly bool, restrict bool, allowedNames []string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	if restrict && osversion.Get().Build < 18328 {
		return errors.New("single-file mappings are not supported on this build of Windows")
	}
	if uvmPath == "" {
		return fmt.Errorf("uvmPath must be passed to AddPlan9")
	}

	// TODO: JTERRY75 - These are marked private in the schema. For now use them
	// but when there are public variants we need to switch to them.
	const (
		shareFlagsReadOnly           int32 = 0x00000001
		shareFlagsLinuxMetadata      int32 = 0x00000004
		shareFlagsCaseSensitive      int32 = 0x00000008
		shareFlagsRestrictFileAccess int32 = 0x00000080
	)

	// TODO: JTERRY75 - `shareFlagsCaseSensitive` only works if the Windows
	// `hostPath` supports case sensitivity. We need to detect this case before
	// forwarding this flag in all cases.
	flags := shareFlagsLinuxMetadata // | shareFlagsCaseSensitive
	if readOnly {
		flags |= shareFlagsReadOnly
	}
	if restrict {
		flags |= shareFlagsRestrictFileAccess
	}

	modification := &hcsschema.ModifySettingRequest{
		RequestType: requesttype.Add,
		Settings: hcsschema.Plan9Share{
			Name:         shareName,
			AccessName:   shareName,
			Path:         hostPath,
			Port:         plan9Port,
			Flags:        flags,
			AllowedFiles: allowedNames,
		},
		ResourcePath: "VirtualMachine/Devices/Plan9/Shares",
		GuestRequest: guestrequest.GuestRequest{
			ResourceType: guestrequest.ResourceTypeMappedDirectory,
			RequestType:  requesttype.Add,
			Settings: guestrequest.LCOWMappedDirectory{
				MountPath: uvmPath,
				ShareName: shareName,
				Port:      plan9Port,
				ReadOnly:  readOnly,
			},
		},
	}

	if err := vm.modifySetting(ctx, modification); err != nil {
		return err
	}

	return nil
}

func (vm *VirtualMachineSpec) RemovePlan9(shareName string, uvmPath string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	modification := &hcsschema.ModifySettingRequest{
		RequestType: requesttype.Remove,
		Settings: hcsschema.Plan9Share{
			Name:       shareName,
			AccessName: shareName,
			Port:       plan9Port,
		},
		ResourcePath: "VirtualMachine/Devices/Plan9/Shares",
		GuestRequest: guestrequest.GuestRequest{
			ResourceType: guestrequest.ResourceTypeMappedDirectory,
			RequestType:  requesttype.Remove,
			Settings: guestrequest.LCOWMappedDirectory{
				MountPath: uvmPath,
				ShareName: shareName,
				Port:      plan9Port,
			},
		},
	}
	if err := vm.modifySetting(ctx, modification); err != nil {
		return fmt.Errorf("failed to remove plan9 share %s from %s: %+v: %s", shareName, vm.ID, modification, err)
	}
	return nil
}

func (vm *VirtualMachineSpec) UpdateGpuConfiguration(mode GpuAssignmentMode, allowVendorExtension bool, assignments map[string]uint16) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}
	defer system.Close()

	settings := hcsschema.GpuConfiguration{
		AssignmentMode:       string(mode),
		AllowVendorExtension: allowVendorExtension,
	}

	if len(assignments) != 0 {
		settings.AssignmentRequest = assignments
	}

	request := &hcsschema.ModifySettingRequest{
		RequestType:  requesttype.Update,
		ResourcePath: "VirtualMachine/ComputeTopology/Gpu",
		Settings:     settings,
	}

	if err := vm.modifySetting(ctx, request); err != nil {
		return err
	}

	return nil
}

// Add vPCI device
func (vm *VirtualMachineSpec) AssignDevice(ctx context.Context, deviceID string) (string, error) {
	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return "", err
	}
	defer system.Close()

	guid, err := guid.NewV4()
	if err != nil {
		return "", err
	}

	vmBusGUID := guid.String()
	targetDevice := hcsschema.VirtualPciDevice{
		Functions: []hcsschema.VirtualPciFunction{
			{
				DeviceInstancePath: deviceID,
			},
		},
	}
	request := &hcsschema.ModifySettingRequest{
		ResourcePath: fmt.Sprintf("VirtualMachine/Devices/VirtualPci/%s", vmBusGUID),
		RequestType:  requesttype.Add,
		Settings:     targetDevice,
	}

	// for LCOW, we need to make sure that specific paths relating to the
	// device exist so they are ready to be used by later
	// work in openGCS
	request.GuestRequest = guestrequest.GuestRequest{
		ResourceType: guestrequest.ResourceTypeVPCIDevice,
		RequestType:  requesttype.Add,
		Settings: guestrequest.LCOWMappedVPCIDevice{
			VMBusGUID: vmBusGUID,
		},
	}

	if err := vm.modifySetting(ctx, request); err != nil {
		return "", err
	}

	return vmBusGUID, nil
}

// Removes a vpci device from VirtualMachineSpec
func (vm *VirtualMachineSpec) RemoveDevice(ctx context.Context, vmBusGUID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	system, err := hcs.OpenComputeSystem(ctx, vm.ID)
	if err != nil {
		return err
	}

	defer system.Close()

	return vm.modifySetting(ctx, &hcsschema.ModifySettingRequest{
		ResourcePath: fmt.Sprintf("VirtualMachine/Devices/VirtualPci/%s", vmBusGUID),
		RequestType:  requesttype.Remove,
	})
}

func (vm *VirtualMachineSpec) String() string {
	jsonString, err := json.Marshal(vm.spec)
	if err != nil {
		return ""
	}

	return string(jsonString)
}

func (vm *VirtualMachineSpec) shutdownThroughGuestConnection(ctx context.Context, force bool) (err error) {
	return vm.gc.Shutdown(ctx, force)
}

func (vm *VirtualMachineSpec) modifySetting(ctx context.Context, doc *hcsschema.ModifySettingRequest) (err error) {
	if doc.GuestRequest == nil || vm.gc == nil {
		return vm.system.Modify(ctx, doc)
	}

	hostdoc := *doc
	hostdoc.GuestRequest = nil
	if doc.ResourcePath != "" && doc.RequestType == requesttype.Add {
		err = vm.system.Modify(ctx, &hostdoc)
		if err != nil {
			return fmt.Errorf("adding VM resources: %s", err)
		}
		defer func() {
			if err != nil {
				hostdoc.RequestType = requesttype.Remove
				rerr := vm.system.Modify(ctx, &hostdoc)
				if rerr != nil {
					log.G(ctx).WithError(err).Error("failed to roll back resource add")
				}
			}
		}()
	}
	err = vm.gc.Modify(ctx, doc.GuestRequest)
	if err != nil {
		return fmt.Errorf("guest modify: %s", err)
	}
	if doc.ResourcePath != "" && doc.RequestType == requesttype.Remove {
		err = vm.system.Modify(ctx, &hostdoc)
		if err != nil {
			err = fmt.Errorf("removing VM resources: %s", err)
			log.G(ctx).WithError(err).Error("failed to remove host resources after successful guest request")
			return err
		}
	}
	return nil
}

func (vm *VirtualMachineSpec) listenVsock(port uint32) (net.Listener, error) {
	return winio.ListenHvsock(&winio.HvsockAddr{
		VMID:      vm.runtimeId,
		ServiceID: winio.VsockServiceID(port),
	})
}

// acceptAndClose accepts a connection and then closes a listener. If the
// context becomes done or the VM terminates, the operation will be
// cancelled (but the listener will still be closed).
func (vm *VirtualMachineSpec) acceptAndClose(ctx context.Context, l net.Listener) (net.Conn, error) {
	var conn net.Conn
	ch := make(chan error)
	go func() {
		var err error
		conn, err = l.Accept()
		ch <- err
	}()

	select {
	case err := <-ch:
		l.Close()
		return conn, err
	case <-ctx.Done():
	}
	l.Close()
	err := <-ch
	if err == nil {
		return conn, err
	}

	// Prefer context error to VM error to accept error in order to return the
	// most useful error.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, err
}

func generateShutdownOptions(force bool) (string, error) {
	// TODO: shutdown options only supported at schema version 2.5 and above
	//       check current schema version on the running system and return empty string if
	//       running on schema version 2.4 and below
	options := hcsschema.ShutdownOptions{
		Mechanism: string(ShutdownMechanismGuestConnection),
		Type:      string(ShutdownTypeShutdown),
		Force:     force,
		Reason:    "Requested shutdown",
	}
	optionsB, err := json.Marshal(options)
	if err != nil {
		return "", err
	}
	return string(optionsB), nil
}

func getSchemaVersion(opts *VirtualMachineOptions) hcsschema.Version {
	if opts.SecureBootEnabled || opts.HighMmioBaseInMB != 0 || opts.HighMmioGapInMB != 0 {
		return hcsschema.Version{
			Major: 2,
			Minor: 3,
		}
	}

	return hcsschema.Version{
		Major: 2,
		Minor: 1,
	}
}
