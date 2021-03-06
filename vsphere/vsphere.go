// Package vsphere is a vmkite-specific abstraction over vmware/govmomi,
// providing the ability to query and create vmkite macOS VMs using the VMware
// vSphere API.
package vsphere

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
)

const keepAliveDuration = time.Second * 30

// ConnectionParams is passed by calling code to NewSession()
type ConnectionParams struct {
	Host     string
	User     string
	Pass     string
	Insecure bool
}

// Session holds state for a vSphere session;
// client connection, context, session-cached values
type Session struct {
	client     *govmomi.Client
	ctx        context.Context
	datacenter *object.Datacenter
	finder     *find.Finder
}

// VirtualMachineCreationParams is passed by calling code to Session.CreateVM()
type VirtualMachineCreationParams struct {
	BuildkiteAgentToken string
	ClusterPath         string
	VirtualMachinePath  string
	DatastoreName       string
	GuestID             string
	MemoryMB            int64
	Name                string
	NetworkLabel        string
	NumCPUs             int32
	NumCoresPerSocket   int32
	SrcDiskDataStore    string
	SrcDiskPath         string
	GuestInfo           map[string]string
}

// NewSession logs in to a new Session based on ConnectionParams
func NewSession(ctx context.Context, cp ConnectionParams) (*Session, error) {
	sess := &Session{
		ctx: ctx,
	}
	return sess, sess.connect(ctx, cp)
}

// Connect to vSphere API, with keep-alive
// See https://github.com/vmware/vic/blob/master/pkg/vsphere/session/session.go#L191
func (s *Session) connect(ctx context.Context, cp ConnectionParams) error {
	u, err := url.Parse(fmt.Sprintf("https://%s/sdk", cp.Host))
	if err != nil {
		return err
	}

	u.User = url.UserPassword(cp.User, cp.Pass)
	soapClient := soap.NewClient(u, cp.Insecure)
	soapClient.Version = "6.0" // Pin to 6.0 until we need 6.5+ specific API

	var login = func(ctx context.Context) error {
		return s.client.Login(ctx, u.User)
	}

	vimClient, err := vim25.NewClient(ctx, soapClient)
	if err != nil {
		return err
	}

	vimClient.RoundTripper = session.KeepAliveHandler(soapClient, keepAliveDuration,
		func(roundTripper soap.RoundTripper) error {
			_, err := methods.GetCurrentTime(context.Background(), roundTripper)
			if err == nil {
				return nil
			}

			debugf("session keepalive error: %s", err)
			if isNotAuthenticated(err) {
				if err = login(ctx); err != nil {
					debugf("session keepalive failed to re-authenticate: %s", err)
				} else {
					debugf("session keepalive re-authenticated")
				}
			}

			return nil
		})

	s.client = &govmomi.Client{
		Client:         vimClient,
		SessionManager: session.NewManager(vimClient),
	}

	return login(ctx)
}

func (vs *Session) VirtualMachine(path string) (*VirtualMachine, error) {
	finder, err := vs.getFinder()
	if err != nil {
		return nil, err
	}
	debugf("finder.VirtualMachine(%v)", path)
	vm, err := finder.VirtualMachine(vs.ctx, path)
	if err != nil {
		return nil, err
	}
	return &VirtualMachine{
		vs:   vs,
		mo:   vm,
		Name: vm.Name(),
	}, nil
}

// CreateVM launches a new macOS VM based on VirtualMachineCreationParams
func (vs *Session) CreateVM(params VirtualMachineCreationParams) (*VirtualMachine, error) {
	finder, err := vs.getFinder()
	if err != nil {
		return nil, err
	}
	folder, err := vs.vmFolder()
	if err != nil {
		return nil, err
	}
	debugf("finder.ClusterComputeResource(%s)", params.ClusterPath)
	cluster, err := finder.ClusterComputeResource(vs.ctx, params.ClusterPath)
	if err != nil {
		return nil, err
	}
	debugf("cluster.ResourcePool()")
	resourcePool, err := cluster.ResourcePool(vs.ctx)
	if err != nil {
		return nil, err
	}
	configSpec, err := vs.createConfigSpec(params)
	if err != nil {
		return nil, err
	}
	debugf("folder.CreateVM %s on %s", params.Name, resourcePool)
	task, err := folder.CreateVM(vs.ctx, configSpec, resourcePool, nil)
	if err != nil {
		return nil, err
	}
	debugf("waiting for CreateVM %v", task)
	if err := task.Wait(vs.ctx); err != nil {
		return nil, err
	}
	vm, err := vs.VirtualMachine(folder.InventoryPath + "/" + params.Name)
	if err != nil {
		return nil, err
	}
	return vm, nil
}

func (vs *Session) vmFolder() (*object.Folder, error) {
	if vs.datacenter == nil {
		return nil, errors.New("datacenter not loaded")
	}
	dcFolders, err := vs.datacenter.Folders(vs.ctx)
	if err != nil {
		return nil, err
	}
	return dcFolders.VmFolder, nil
}

func (vs *Session) createConfigSpec(params VirtualMachineCreationParams) (cs types.VirtualMachineConfigSpec, err error) {
	devices, err := addEthernet(nil, vs, params.NetworkLabel)
	if err != nil {
		return
	}

	devices, err = addSCSI(devices)
	if err != nil {
		return
	}

	devices, err = addDisk(devices, vs, params)
	if err != nil {
		return
	}

	devices, err = addUSB(devices)
	if err != nil {
		return
	}

	deviceChange, err := devices.ConfigSpec(types.VirtualDeviceConfigSpecOperationAdd)
	if err != nil {
		return
	}

	extraConfig := []types.BaseOptionValue{
		&types.OptionValue{Key: "guestinfo.vmkite-buildkite-agent-token", Value: params.BuildkiteAgentToken},
		&types.OptionValue{Key: "guestinfo.vmkite-name", Value: params.Name},
		&types.OptionValue{Key: "guestinfo.vmkite-vmdk", Value: params.SrcDiskPath},
	}

	if params.GuestInfo != nil {
		for key, val := range params.GuestInfo {
			debugf("setting guestinfo.%s=%q", key, val)
			extraConfig = append(extraConfig,
				&types.OptionValue{Key: "guestinfo." + key, Value: val},
			)
		}
	}

	// ensure a consistent pci slot for the ethernet card, helps systemd
	extraConfig = append(extraConfig,
		&types.OptionValue{Key: "ethernet0.pciSlotNumber", Value: "32"},
	)

	finder, err := vs.getFinder()
	if err != nil {
		return
	}
	debugf("finder.Datastore(%s)", params.DatastoreName)
	ds, err := finder.Datastore(vs.ctx, params.DatastoreName)
	if err != nil {
		return
	}
	fileInfo := &types.VirtualMachineFileInfo{
		VmPathName: fmt.Sprintf("[%s]", ds.Name()),
	}

	t := true
	cs = types.VirtualMachineConfigSpec{
		DeviceChange:        deviceChange,
		ExtraConfig:         extraConfig,
		Files:               fileInfo,
		GuestId:             params.GuestID,
		MemoryMB:            params.MemoryMB,
		Name:                params.Name,
		NestedHVEnabled:     &t,
		NumCPUs:             params.NumCPUs,
		NumCoresPerSocket:   params.NumCoresPerSocket,
		VirtualICH7MPresent: &t,
		VirtualSMCPresent:   &t,
	}

	return
}

func addEthernet(devices object.VirtualDeviceList, vs *Session, label string) (object.VirtualDeviceList, error) {
	finder, err := vs.getFinder()
	if err != nil {
		return nil, err
	}
	path := "*" + label
	debugf("finder.Network(%s)", path)
	network, err := finder.Network(vs.ctx, path)
	if err != nil {
		return nil, err
	}
	backing, err := network.EthernetCardBackingInfo(vs.ctx)
	if err != nil {
		return nil, err
	}
	device, err := object.EthernetCardTypes().CreateEthernetCard("vmxnet3", backing)
	if err != nil {
		return nil, err
	}
	card := device.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()
	card.AddressType = string(types.VirtualEthernetCardMacTypeGenerated)

	return append(devices, device), nil
}

func addSCSI(devices object.VirtualDeviceList) (object.VirtualDeviceList, error) {
	scsi, err := object.SCSIControllerTypes().CreateSCSIController("scsi")
	if err != nil {
		return nil, err
	}
	return append(devices, scsi), nil
}

func addDisk(devices object.VirtualDeviceList, vs *Session, params VirtualMachineCreationParams) (object.VirtualDeviceList, error) {
	finder, err := vs.getFinder()
	if err != nil {
		return nil, err
	}

	debugf("finder.Datastore(%s)", params.SrcDiskDataStore)
	diskDatastore, err := finder.Datastore(vs.ctx, params.SrcDiskDataStore)
	if err != nil {
		return nil, err
	}

	controller, err := devices.FindDiskController("scsi")
	if err != nil {
		return nil, err
	}

	disk := devices.CreateDisk(
		controller,
		diskDatastore.Reference(),
		diskDatastore.Path(params.SrcDiskPath),
	)

	backing := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo)
	backing.ThinProvisioned = types.NewBool(true)
	backing.DiskMode = string(types.VirtualDiskModeIndependent_nonpersistent)

	return append(devices, disk), nil
}

func addUSB(devices object.VirtualDeviceList) (object.VirtualDeviceList, error) {
	t := true
	usb := &types.VirtualUSBController{AutoConnectDevices: &t, EhciEnabled: &t}
	return append(devices, usb), nil
}

func (vs *Session) getFinder() (*find.Finder, error) {
	if vs.finder == nil {
		debugf("find.NewFinder()")
		finder := find.NewFinder(vs.client.Client, true)
		debugf("finder.DefaultDatacenter()")
		dc, err := finder.DefaultDatacenter(vs.ctx)
		if err != nil {
			return nil, err
		}
		debugf("finder.SetDatacenter(%v)", dc)
		finder.SetDatacenter(dc)
		vs.datacenter = dc
		vs.finder = finder
	}
	return vs.finder, nil
}

func debugf(format string, data ...interface{}) {
	log.Printf("[vsphere] "+format, data...)
}

func isNotAuthenticated(err error) bool {
	if soap.IsSoapFault(err) {
		switch soap.ToSoapFault(err).VimFault().(type) {
		case types.NotAuthenticated:
			return true
		}
	}
	return false
}
