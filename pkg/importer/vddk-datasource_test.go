//go:build amd64
// +build amd64

package importer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"net/url"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/soap"
	"github.com/vmware/govmomi/vim25/types"
	v1 "k8s.io/api/core/v1"

	"kubevirt.io/containerized-data-importer/pkg/common"
	"kubevirt.io/containerized-data-importer/pkg/image"
	libnbd "libguestfs.org/libnbd"
)

const (
	socketPath = "nbd://nbdtest.sock"
)

type mockNbdExport struct {
	Size func() (uint64, error)
	Read func(uint64) ([]byte, error)
}

func defaultMockNbdExport() mockNbdExport {
	export := &mockNbdExport{}
	export.Size = func() (uint64, error) {
		return 12345, nil
	}
	export.Read = func(uint64) ([]byte, error) {
		return bytes.Repeat([]byte{0}, 512), nil
	}
	return *export
}

var currentExport mockNbdExport

var _ = Describe("VDDK data source", func() {
	BeforeEach(func() {
		mockSinkBuffer = bytes.Repeat([]byte{0x00}, 512)
		newVddkDataSource = createMockVddkDataSource
		newVddkDataSink = createMockVddkDataSink
		newVMwareClient = createMockVMwareClient
		newNbdKitWrapper = createMockNbdKitWrapper
		newTerminationChannel = createMockTerminationChannel
		currentExport = defaultMockNbdExport()
		currentVMwareFunctions = defaultMockVMwareFunctions()
	})

	AfterEach(func() {
		newVddkDataSource = createVddkDataSource
	})

	It("NewVDDKDataSource should fail when called with an invalid endpoint", func() {
		newVddkDataSource = createVddkDataSource
		newVMwareClient = createVMwareClient
		_, err := NewVDDKDataSource("httpx://-------", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).To(HaveOccurred())
	})

	It("NewVDDKDataSource should not fail on credentials with special characters", func() {
		newVddkDataSource = createVddkDataSource
		newVMwareClient = createVMwareClient
		_, err := NewVDDKDataSource("http://--------", "test#user@vsphere.local", "Test#password", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("no such host"))
		Expect(err.Error()).ToNot(ContainSubstring("Test#password"))
		Expect(err.Error()).ToNot(ContainSubstring(url.PathEscape("Test#password")))
	})

	It("VDDK data source GetURL should pass through NBD socket information", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		socket := dp.GetURL()
		path := socket.String()
		Expect(path).To(Equal(socketPath))
	})

	It("VDDK data source should move to transfer data phase after Info", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		phase, err := dp.Info()
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseTransferDataFile))
	})

	It("VDDK data source should move to preallocate phase after TransferFile", func() {
		replaceExport := currentExport
		replaceExport.Size = func() (uint64, error) {
			return 512, nil
		}
		replaceExport.Read = func(uint64) ([]byte, error) {
			return bytes.Repeat([]byte{0x55}, 512), nil
		}
		currentExport = replaceExport
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		phase, err := dp.Info()
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseTransferDataFile))
		phase, err = dp.TransferFile("")
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseResize))
	})

	It("VDDK data source should fail if TransferFile fails", func() {
		newVddkDataSink = createVddkDataSink
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		phase, err := dp.Info()
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseTransferDataFile))
		phase, err = dp.TransferFile("")
		Expect(err).To(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseError))
	})

	It("VDDK data source should know if it is a delta copy", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "checkpoint-1", "checkpoint-2", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(dp.IsDeltaCopy()).To(BeTrue())
	})

	It("VDDK data source should know if it is not a delta copy", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(dp.IsDeltaCopy()).To(BeFalse())
	})

	It("VDDK delta copy should return immediately if there are no changed blocks", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "checkpoint-1", "checkpoint-2", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		dp.ChangedBlocks = &types.DiskChangeInfo{
			StartOffset: 0,
			Length:      0,
			ChangedArea: []types.DiskChangeExtent{},
		}
		phase, err := dp.TransferFile("")
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseComplete))
	})

	It("VDDK full copy should successfully copy the same bytes passed in", func() {
		dp, err := NewVDDKDataSource("", "", "", "", "", "", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		dp.Size = 40 << 20
		sourceBytes := bytes.Repeat([]byte{0x55}, int(dp.Size))
		replaceExport := currentExport
		replaceExport.Size = func() (uint64, error) {
			return dp.Size, nil
		}
		replaceExport.Read = func(uint64) ([]byte, error) {
			return sourceBytes, nil
		}
		currentExport = replaceExport

		mockSinkBuffer = bytes.Repeat([]byte{0x00}, int(dp.Size))

		phase, err := dp.TransferFile("target")
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseResize))

		sourceSum := md5.Sum(sourceBytes)
		destSum := md5.Sum(mockSinkBuffer)
		Expect(sourceSum).To(Equal(destSum))
	})

	It("VDDK delta copy should successfully apply a delta to a base disk image", func() {

		// Copy base disk ("snapshot 1")
		snap1, err := NewVDDKDataSource("", "", "", "", "", "", "checkpoint-1", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		snap1.Size = 40 << 20
		sourceBytes := bytes.Repeat([]byte{0x55}, int(snap1.Size))
		replaceExport := currentExport
		replaceExport.Size = func() (uint64, error) {
			return snap1.Size, nil
		}
		replaceExport.Read = func(uint64) ([]byte, error) {
			return sourceBytes, nil
		}
		currentExport = replaceExport

		mockSinkBuffer = bytes.Repeat([]byte{0x00}, int(snap1.Size))

		phase, err := snap1.TransferFile("target")
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseResize))

		sourceSum := md5.Sum(sourceBytes)
		destSum := md5.Sum(mockSinkBuffer)
		Expect(sourceSum).To(Equal(destSum))

		// Write some data to the first snapshot, then copy the delta from difference between the two snapshots
		snap2, err := NewVDDKDataSource("", "", "", "", "", "", "checkpoint-1", "checkpoint-2", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		snap2.Size = 40 << 20
		copy(sourceBytes[1024:2048], bytes.Repeat([]byte{0xAA}, 1024))
		snap2.ChangedBlocks = &types.DiskChangeInfo{
			StartOffset: 1024,
			Length:      1024,
			ChangedArea: []types.DiskChangeExtent{{
				Start:  1024,
				Length: 1024,
			}},
		}
		changedSourceSum := md5.Sum(sourceBytes)

		phase, err = snap2.TransferFile(".")
		Expect(err).ToNot(HaveOccurred())
		Expect(phase).To(Equal(ProcessingPhaseResize))

		deltaSum := md5.Sum(mockSinkBuffer)
		Expect(changedSourceSum).To(Equal(deltaSum))
	})

	It("VDDK delta copy should accept a change ID as a checkpoint", func() {
		diskName := "disk"
		snapshotName := "checkpoint-2"
		changeID := "52 de c0 d9 b9 43 9d 10-61 d5 4c 1b e9 7b 65 63/81"
		newVddkDataSource = createVddkDataSource

		snapshots := createSnapshots(snapshotName, "")
		currentVMwareFunctions.FindSnapshot = func(ctx context.Context, nameOrID string) (*types.ManagedObjectReference, error) {
			return &snapshots.RootSnapshotList[0].Snapshot, nil
		}
		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(diskName, 12345)
				} else if property[0] == "snapshot" {
					out.Snapshot = snapshots
				}
			case *mo.VirtualMachineSnapshot:
				out.Config = *createVirtualDiskConfig("snapshotdisk", 123456)
			}
			return nil
		}

		changeInfo := types.DiskChangeInfo{
			StartOffset: 100,
			Length:      1000,
			ChangedArea: []types.DiskChangeExtent{
				{
					Start:  0,
					Length: 1000,
				},
			},
		}
		QueryChangedDiskAreas = func(ctx context.Context, r soap.RoundTripper, req *types.QueryChangedDiskAreas) (*types.QueryChangedDiskAreasResponse, error) {
			return &types.QueryChangedDiskAreasResponse{
				Returnval: changeInfo,
			}, nil
		}

		ds, err := NewVDDKDataSource("", "", "", "", "", diskName, snapshotName, changeID, "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(ds.ChangedBlocks).To(Equal(&changeInfo))
	})

	DescribeTable("disk name lookup", func(targetDiskName, diskName, snapshotDiskName, rootSnapshotParentName string, expectedSuccess bool) {
		var returnedDiskName string

		newVddkDataSource = createVddkDataSource
		newNbdKitWrapper = func(vmware *VMwareClient, fileName string) (*NbdKitWrapper, error) {
			returnedDiskName = fileName
			return createMockNbdKitWrapper(vmware, fileName)
		}
		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(diskName, 12345)
				} else if property[0] == "snapshot" {
					out.Snapshot = createSnapshots("snapshot-1", "snapshot-2")
				}
			case *mo.VirtualMachineSnapshot:
				out.Config = *createVirtualDiskConfig(snapshotDiskName, 123456)
				disk := out.Config.Hardware.Device[0].(*types.VirtualDisk)
				parent := disk.Backing.(*types.VirtualDiskFlatVer1BackingInfo).Parent
				parent.FileName = rootSnapshotParentName
			}
			return nil
		}

		_, err := NewVDDKDataSource("http://vcenter.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", targetDiskName, "", "", "", v1.PersistentVolumeFilesystem)
		if expectedSuccess {
			Expect(err).ToNot(HaveOccurred())
			Expect(returnedDiskName).To(Equal(targetDiskName))
		} else {
			Expect(err).To(HaveOccurred())
			Expect(returnedDiskName).ToNot(Equal(targetDiskName))
		}
	},
		Entry("should find backing file on a VM", "[teststore] testvm/testfile.vmdk", "[teststore] testvm/testfile.vmdk", "", "", true),
		Entry("should find backing file on a snapshot", "[teststore] testvm/testfile.vmdk", "wrong disk.vmdk", "[teststore] testvm/testfile.vmdk", "", true),
		Entry("should find base backing file even if not listed as first snapshot", "[teststore] testvm/testfile.vmdk", "[teststore] testvm/testfile-000001.vmdk", "[teststore] testvm/testfile-000002.vmdk", "[teststore] testvm/testfile.vmdk", true),
		Entry("should fail if backing file is not found in snapshot tree", "[teststore] testvm/testfile.vmdk", "wrong disk 1.vmdk", "wrong disk 2.vmdk", "wrong disk 1.vmdk", false),
	)

	It("should find two snapshots and get a list of changed blocks", func() {
		newVddkDataSource = createVddkDataSource
		diskName := "testdisk.vmdk"

		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(diskName, 12345)
				} else if property[0] == "snapshot" {
					out.Snapshot = createSnapshots("snapshot-1", "snapshot-2")
				}
			case *mo.VirtualMachineSnapshot:
				out.Config = *createVirtualDiskConfig("testdisk-00001.vmdk", 123456)
			}
			return nil
		}

		snapshots := createSnapshots("snapshot-1", "snapshot-2")
		snapshotList := []*types.ManagedObjectReference{
			&snapshots.RootSnapshotList[0].Snapshot,
			&snapshots.RootSnapshotList[0].ChildSnapshotList[0].Snapshot,
		}

		currentVMwareFunctions.FindSnapshot = func(ctx context.Context, nameOrID string) (*types.ManagedObjectReference, error) {
			for _, snap := range snapshotList {
				if snap.Value == nameOrID {
					return snap, nil
				}
			}
			return nil, errors.New("could not find snapshot")
		}

		changedBlockList := types.DiskChangeInfo{
			StartOffset: 0,
			Length:      10240,
			ChangedArea: []types.DiskChangeExtent{
				{
					Start:  1024,
					Length: 512,
				},
				{
					Start:  4096,
					Length: 4096,
				},
			},
		}
		currentVMwareFunctions.QueryChangedDiskAreas = func(ctx context.Context, baseSnapshot *types.ManagedObjectReference, changedSnapshot *types.ManagedObjectReference, disk *types.VirtualDisk, offset int64) (types.DiskChangeInfo, error) {
			return changedBlockList, nil
		}

		// Expect source.ChangedBlocks to equal local changed blocks
		source, err := NewVDDKDataSource("http://vcenter.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", diskName, "snapshot-1", "snapshot-2", "false", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(changedBlockList.StartOffset).To(Equal(source.ChangedBlocks.StartOffset))
		Expect(changedBlockList.Length).To(Equal(source.ChangedBlocks.Length))
		for index, extent := range changedBlockList.ChangedArea {
			Expect(extent.Start).To(Equal(source.ChangedBlocks.ChangedArea[index].Start))
			Expect(extent.Length).To(Equal(source.ChangedBlocks.ChangedArea[index].Length))
		}
	})

	It("should not crash when the disk is not found and has no snapshots", func() {
		newVddkDataSource = createVddkDataSource
		diskName := "testdisk.vmdk"
		wrongDiskName := "wrong.vmdk"

		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(wrongDiskName, 12345)
				}
			}
			return nil
		}

		_, err := NewVDDKDataSource("http://vcenter.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", diskName, "", "", "false", v1.PersistentVolumeFilesystem)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(Equal("disk 'testdisk.vmdk' is not present in VM hardware config or snapshot list"))
	})

	It("should cancel transfer on SIGTERM", func() {
		newVddkDataSource = createVddkDataSource
		diskName := "testdisk.vmdk"

		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(diskName, 12345)
				}
			}
			return nil
		}
		_, err := NewVDDKDataSource("http://vcenter.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", diskName, "snapshot-1", "snapshot-2", "false", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		mockTerminationChannel <- os.Interrupt
		Expect(err).ToNot(HaveOccurred())
	})

	It("should reduce pread length for vCenter endpoints", func() {
		newVddkDataSource = createVddkDataSource
		diskName := "testdisk.vmdk"

		currentVMwareFunctions.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
			switch out := result.(type) {
			case *mo.VirtualMachine:
				if property[0] == "config.hardware.device" {
					out.Config = createVirtualDiskConfig(diskName, 12345)
				}
			}
			return nil
		}
		_, err := NewVDDKDataSource("http://esx.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", diskName, "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(MaxPreadLength).To(Equal(uint32(MaxPreadLengthESX)))
		_, err = NewVDDKDataSource("http://vcenter.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", diskName, "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())
		Expect(MaxPreadLength).To(Equal(uint32(MaxPreadLengthVC)))
	})

	It("GetTerminationMessage should contain VDDK connection information", func() {
		const testVersion = "testVersion"
		const testHost = "testHost"

		source, err := NewVDDKDataSource("http://esx.test", "user", "pass", "aa:bb:cc:dd", "1-2-3-4", "testdisk.vmdk", "", "", "", v1.PersistentVolumeFilesystem)
		Expect(err).ToNot(HaveOccurred())

		vddkVersion = testVersion
		vddkHost = testHost
		Expect(*source.GetTerminationMessage()).To(Equal(common.TerminationMessage{VddkInfo: &common.VddkInfo{Version: testVersion, Host: testHost}}))
	})
})

var _ = Describe("VDDK log watcher", func() {
	DescribeTable("should set VDDK connection information from nbdkit log stream, ", func(messages []string, results []string) {
		vddkVersion = "not set"
		vddkHost = "not set"
		logbuffer := new(bytes.Buffer)
		logreader := bufio.NewReader(logbuffer)
		watcher := createNbdKitLogWatcher()
		for _, message := range messages {
			logbuffer.WriteString(message + "\n")
		}
		watcher.Start(logreader)
		watcher.Stop()
		Expect(vddkVersion).To(Equal(results[0]))
		Expect(vddkHost).To(Equal(results[1]))
	},
		Entry("with no data", []string{}, []string{"not set", "not set"}),
		Entry("with good version string", []string{"nbdkit: debug: VMware VixDiskLib (7.0.0) Release build-15832853"}, []string{"7.0.0", "not set"}),
		Entry("with alternate good version string", []string{"nbdkit: debug: VMware VixDiskLib (7.0) Release build-15832853"}, []string{"7.0", "not set"}),
		Entry("with bad version string", []string{"nbdkit: debug: VMware VixDiskLib 7.0 Release build-15832853"}, []string{"not set", "not set"}),
		Entry("with good host string", []string{"nbdkit: vddk[1]: debug: DISKLIB-LINK  : Opened 'vpxa-nfcssl://[iSCSI_Datastore] test/test.vmdk@esx15.test.lan:902' (0xa): custom, 50331648 sectors / 24 GB."}, []string{"not set", "esx15.test.lan"}),
		Entry("with alternate good host string", []string{"nbdkit: vddk[1]: debug: DISKLIB-LINK  : Opened 'http://disk@esx:1234' (0xa): custom, 50331648 sectors / 24 GB."}, []string{"not set", "esx"}),
		Entry("with bad host string", []string{"nbdkit: vddk[1]: debug: DISKLIB-LINK  : Opened 'vpxa-nfcssl://esx' (0xa): custom, 50331648 sectors / 24 GB."}, []string{"not set", "not set"}),
		Entry("with good version and host strings", []string{
			"nbdkit: debug: VMware VixDiskLib (7.0.0) Release build-15832853",
			"nbdkit: vddk[1]: debug: DISKLIB-LINK  : Opened 'vpxa-nfcssl://disk@esx:1234' (0xa): custom, 50331648 sectors / 24 GB."},
			[]string{"7.0.0", "esx"}),
		Entry("with bad version and host strings", []string{
			"nbdkit: debug: VMware VixDiskLib ",
			"nbdkit: vddk[1]: debug: DISKLIB-LINK  : Opened '"},
			[]string{"not set", "not set"}),
	)
})

type mockNbdOperations struct{}

func (handle *mockNbdOperations) GetSize() (uint64, error) {
	return currentExport.Size()
}

func (handle *mockNbdOperations) Pread(buf []byte, offset uint64, optargs *libnbd.PreadOptargs) error {

	fakebuf, err := currentExport.Read(offset)
	copy(buf, fakebuf[offset:offset+uint64(len(buf))])
	return err
}

func (handle *mockNbdOperations) Close() *libnbd.LibnbdError {
	return nil
}

func (handle *mockNbdOperations) BlockStatus(length uint64, offset uint64, callback libnbd.ExtentCallback, optargs *libnbd.BlockStatusOptargs) error {
	err := 0
	callback("base:allocation", offset, []uint32{uint32(length), 0}, &err)
	return nil
}

func createMockVddkDataSource(endpoint string, accessKey string, secKey string, thumbprint string, uuid string, backingFile string, currentCheckpoint string, previousCheckpoint string, finalCheckpoint string, volumeMode v1.PersistentVolumeMode) (*VDDKDataSource, error) {
	socketURL, err := url.Parse(socketPath)
	if err != nil {
		return nil, err
	}

	handle := &mockNbdOperations{}

	nbdkit := &NbdKitWrapper{
		n:      &image.Nbdkit{},
		Socket: socketURL,
		Handle: handle,
	}

	return &VDDKDataSource{
		NbdKit:           nbdkit,
		ChangedBlocks:    nil,
		CurrentSnapshot:  currentCheckpoint,
		PreviousSnapshot: previousCheckpoint,
		Size:             0,
		VolumeMode:       volumeMode,
	}, nil
}

var mockSinkBuffer []byte

type mockVddkDataSink struct {
	position int
}

func (sink *mockVddkDataSink) ZeroRange(offset uint64, length uint32) error {
	buf := bytes.Repeat([]byte{0x00}, int(length))
	_, err := sink.Pwrite(buf, offset)
	return err
}

func (sink *mockVddkDataSink) Pwrite(buf []byte, offset uint64) (int, error) {
	copy(mockSinkBuffer[offset:offset+uint64(len(buf))], buf)
	if len(buf) > sink.position {
		sink.position = int(offset) + len(buf)
	}
	return len(buf), nil
}

func (sink *mockVddkDataSink) Write(buf []byte) (int, error) {
	copy(mockSinkBuffer[sink.position:sink.position+len(buf)], buf)
	sink.position += len(buf)
	return len(buf), nil
}

func (sink *mockVddkDataSink) Close() {}

func createMockVddkDataSink(destinationFile string, size uint64, volumeMode v1.PersistentVolumeMode) (VDDKDataSink, error) {
	sink := &mockVddkDataSink{0}
	return sink, nil
}

type mockVMwareConnectionOperations struct {
	Endpoint string
}

func (ops *mockVMwareConnectionOperations) Logout(context.Context) error {
	return nil
}

func (ops *mockVMwareConnectionOperations) IsVC() bool {
	return strings.Contains(ops.Endpoint, "vcenter")
}

type mockVMwareFunctions struct {
	Properties            func(context.Context, types.ManagedObjectReference, []string, interface{}) error
	Reference             func() types.ManagedObjectReference
	FindSnapshot          func(context.Context, string) (*types.ManagedObjectReference, error)
	QueryChangedDiskAreas func(context.Context, *types.ManagedObjectReference, *types.ManagedObjectReference, *types.VirtualDisk, int64) (types.DiskChangeInfo, error)
	Client                func() *vim25.Client
}

func defaultMockVMwareFunctions() mockVMwareFunctions {
	ops := &mockVMwareFunctions{}
	ops.Properties = func(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
		return nil
	}
	ops.Reference = func() types.ManagedObjectReference {
		return types.ManagedObjectReference{}
	}
	ops.FindSnapshot = func(ctx context.Context, nameOrID string) (*types.ManagedObjectReference, error) {
		return nil, nil
	}
	ops.QueryChangedDiskAreas = func(ctx context.Context, baseSnapshot *types.ManagedObjectReference, changedSnapshot *types.ManagedObjectReference, disk *types.VirtualDisk, offset int64) (types.DiskChangeInfo, error) {
		return types.DiskChangeInfo{}, nil
	}
	ops.Client = func() *vim25.Client {
		return &vim25.Client{}
	}
	return *ops
}

var currentVMwareFunctions mockVMwareFunctions

type mockVMwareVMOperations struct{}

func (ops *mockVMwareVMOperations) Properties(ctx context.Context, ref types.ManagedObjectReference, property []string, result interface{}) error {
	return currentVMwareFunctions.Properties(ctx, ref, property, result)
}

func (ops *mockVMwareVMOperations) Reference() types.ManagedObjectReference {
	return currentVMwareFunctions.Reference()
}

func (ops *mockVMwareVMOperations) FindSnapshot(ctx context.Context, nameOrID string) (*types.ManagedObjectReference, error) {
	return currentVMwareFunctions.FindSnapshot(ctx, nameOrID)
}

func (ops *mockVMwareVMOperations) QueryChangedDiskAreas(ctx context.Context, baseSnapshot *types.ManagedObjectReference, changedSnapshot *types.ManagedObjectReference, disk *types.VirtualDisk, offset int64) (types.DiskChangeInfo, error) {
	return currentVMwareFunctions.QueryChangedDiskAreas(ctx, baseSnapshot, changedSnapshot, disk, offset)
}

func (ops *mockVMwareVMOperations) Client() *vim25.Client {
	return currentVMwareFunctions.Client()
}

func createMockVMwareClient(endpoint string, accessKey string, secKey string, thumbprint string, uuid string) (*VMwareClient, error) {
	ep, _ := url.Parse(endpoint)
	ctx, cancel := context.WithCancel(context.Background())

	return &VMwareClient{
		conn:       &mockVMwareConnectionOperations{endpoint},
		cancel:     cancel,
		context:    ctx,
		moref:      "vm-1",
		thumbprint: thumbprint,
		username:   accessKey,
		password:   secKey,
		url:        ep,
		vm:         &mockVMwareVMOperations{},
	}, nil
}

func createMockNbdKitWrapper(vmware *VMwareClient, diskFileName string) (*NbdKitWrapper, error) {
	u, _ := url.Parse("http://vcenter.test")
	return &NbdKitWrapper{
		n:      &image.Nbdkit{},
		Socket: u,
		Handle: &mockNbdOperations{},
	}, nil
}

func createVirtualDiskConfig(fileName string, key int32) *types.VirtualMachineConfigInfo {
	return &types.VirtualMachineConfigInfo{
		Hardware: types.VirtualHardware{
			Device: []types.BaseVirtualDevice{
				&types.VirtualDisk{
					DiskObjectId: "test-1",
					VirtualDevice: types.VirtualDevice{
						Key: key,
						Backing: &types.VirtualDiskFlatVer1BackingInfo{
							VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
								FileName: fileName,
							},
							Parent: &types.VirtualDiskFlatVer1BackingInfo{},
						},
					},
				},
			},
		},
	}
}

func createSnapshots(name1 string, name2 string) *types.VirtualMachineSnapshotInfo {
	return &types.VirtualMachineSnapshotInfo{
		RootSnapshotList: []types.VirtualMachineSnapshotTree{
			{
				Snapshot: types.ManagedObjectReference{
					Type:  "VirtualMachineSnapshot",
					Value: name1,
				},
				ChildSnapshotList: []types.VirtualMachineSnapshotTree{
					{
						Snapshot: types.ManagedObjectReference{
							Type:  "VirtualMachineSnapshot",
							Value: name2,
						},
					},
				},
			},
		},
	}
}
