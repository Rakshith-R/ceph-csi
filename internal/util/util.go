/*
Copyright 2019 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/cloud-provider/volume/helpers"
	"k8s.io/klog"
	"k8s.io/utils/mount"
)

// RoundOffVolSize rounds up given quantity upto chunks of MiB/GiB
func RoundOffVolSize(size int64) int64 {
	size = RoundOffBytes(size)
	// convert size back to MiB for rbd CLI
	return size / helpers.MiB
}

// RoundOffBytes converts roundoff the size
// 1.1Mib will be round off to 2Mib same for GiB
// size less than 1MiB will be round off to 1MiB
func RoundOffBytes(bytes int64) int64 {
	var num int64
	floatBytes := float64(bytes)
	// round off the value if its in decimal
	if floatBytes < helpers.GiB {
		num = int64(math.Ceil(floatBytes / helpers.MiB))
		num *= helpers.MiB
	} else {
		num = int64(math.Ceil(floatBytes / helpers.GiB))
		num *= helpers.GiB
	}
	return num
}

// variables which will be set during the build time
var (
	// GitCommit tell the latest git commit image is built from
	GitCommit string
	// DriverVersion which will be driver version
	DriverVersion string
)

// Config holds the parameters list which can be configured
type Config struct {
	Vtype           string // driver type [rbd|cephfs|liveness]
	Endpoint        string // CSI endpoint
	DriverName      string // name of the driver
	NodeID          string // node id
	InstanceID      string // unique ID distinguishing this instance of Ceph CSI
	MetadataStorage string // metadata persistence method [node|k8s_configmap]
	PluginPath      string // location of cephcsi plugin
	DomainLabels    string // list of domain labels to read from the node

	// cephfs related flags
	MountCacheDir string // mount info cache save dir

	// metrics related flags
	MetricsPath       string        // path of prometheus endpoint where metrics will be available
	HistogramOption   string        // Histogram option for grpc metrics, should be comma separated value, ex:= "0.5,2,6" where start=0.5 factor=2, count=6
	MetricsIP         string        // TCP port for liveness/ metrics requests
	PidLimit          int           // PID limit to configure through cgroups")
	MetricsPort       int           // TCP port for liveness/grpc metrics requests
	PollTime          time.Duration // time interval in seconds between each poll
	PoolTimeout       time.Duration // probe timeout in seconds
	EnableGRPCMetrics bool          // option to enable grpc metrics

	IsControllerServer bool // if set to true start provisoner server
	IsNodeServer       bool // if set to true start node server
	Version            bool // cephcsi version

	// SkipForceFlatten is set to false if the kernel supports mounting of
	// rbd image or the image chain has the deep-flatten feature.
	SkipForceFlatten bool

	// cephfs related flags
	ForceKernelCephFS bool // force to use the ceph kernel client even if the kernel is < 4.17

	// RbdHardMaxCloneDepth is the hard limit for maximum number of nested volume clones that are taken before a flatten occurs
	RbdHardMaxCloneDepth uint

	// RbdSoftMaxCloneDepth is the soft limit for maximum number of nested volume clones that are taken before a flatten occurs
	RbdSoftMaxCloneDepth uint

	// MaxSnapshotsOnImage represents the maximum number of snapshots allowed
	// on rbd image without flattening, once the limit is reached cephcsi will
	// start flattening the older rbd images to allow more snapshots
	MaxSnapshotsOnImage uint
}

// CreatePersistanceStorage creates storage path and initializes new cache
func CreatePersistanceStorage(sPath, metaDataStore, pluginPath string) (CachePersister, error) {
	var err error
	if err = CreateMountPoint(path.Join(sPath, "controller")); err != nil {
		klog.Errorf("failed to create persistent storage for controller: %v", err)
		return nil, err
	}

	if err = CreateMountPoint(path.Join(sPath, "node")); err != nil {
		klog.Errorf("failed to create persistent storage for node: %v", err)
		return nil, err
	}

	cp, err := NewCachePersister(metaDataStore, pluginPath)
	if err != nil {
		klog.Errorf("failed to define cache persistence method: %v", err)
		return nil, err
	}
	return cp, err
}

// ValidateDriverName validates the driver name
func ValidateDriverName(driverName string) error {
	if driverName == "" {
		return errors.New("driver name is empty")
	}

	if len(driverName) > 63 {
		return errors.New("driver name length should be less than 63 chars")
	}
	var err error
	for _, msg := range validation.IsDNS1123Subdomain(strings.ToLower(driverName)) {
		if err == nil {
			err = errors.New(msg)
			continue
		}
		err = fmt.Errorf("%s: %w", msg, err)
	}
	return err
}

// GetKernelVersion returns the version of the running Unix (like) system from the
// 'utsname' structs 'release' component.
func GetKernelVersion() (string, error) {
	utsname := unix.Utsname{}
	err := unix.Uname(&utsname)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(utsname.Release[:]), "\x00"), nil
}

// KernelVersion holds kernel related informations
type KernelVersion struct {
	Version      int
	PatchLevel   int
	SubLevel     int
	ExtraVersion int    // prefix of the part after the first "-"
	Distribution string // component of full extraversion
	Backport     bool   // backports have a fixed version/patchlevel/sublevel
}

// CheckKernelSupport checks the running kernel and comparing it to known
// versions that have support for required features . Distributors of
// enterprise Linux have backported quota support to previous versions. This
// function checks if the running kernel is one of the versions that have the
// feature/fixes backported.
//
// `uname -r` (or Uname().Utsname.Release has a format like 1.2.3-rc.vendor
// This can be slit up in the following components: - version (1) - patchlevel
// (2) - sublevel (3) - optional, defaults to 0 - extraversion (rc) - optional,
// matching integers only - distribution (.vendor) - optional, match against
// whole `uname -r` string
//
// For matching multiple versions, the kernelSupport type contains a backport
// bool, which will cause matching
// version+patchlevel+sublevel+(>=extraversion)+(~distribution)
//
// In case the backport bool is false, a simple check for higher versions than
// version+patchlevel+sublevel is done.
func CheckKernelSupport(release string, supportedVersions []KernelVersion) bool {
	vers := strings.Split(strings.SplitN(release, "-", 2)[0], ".")
	version, err := strconv.Atoi(vers[0])
	if err != nil {
		klog.Errorf("failed to parse version from %s: %v", release, err)
		return false
	}
	patchlevel, err := strconv.Atoi(vers[1])
	if err != nil {
		klog.Errorf("failed to parse patchlevel from %s: %v", release, err)
		return false
	}
	sublevel := 0
	if len(vers) >= 3 {
		sublevel, err = strconv.Atoi(vers[2])
		if err != nil {
			klog.Errorf("failed to parse sublevel from %s: %v", release, err)
			return false
		}
	}
	extra := strings.SplitN(release, "-", 2)
	extraversion := 0
	if len(extra) == 2 {
		// ignore errors, 1st component of extraversion does not need to be an int
		extraversion, err = strconv.Atoi(strings.Split(extra[1], ".")[0])
		if err != nil {
			// "go lint" wants err to be checked...
			extraversion = 0
		}
	}

	// compare running kernel against known versions
	for _, kernel := range supportedVersions {
		if !kernel.Backport {
			// deal with the default case(s), find >= match for version, patchlevel, sublevel
			if version > kernel.Version || (version == kernel.Version && patchlevel > kernel.PatchLevel) ||
				(version == kernel.Version && patchlevel == kernel.PatchLevel && sublevel >= kernel.SubLevel) {
				return true
			}
		} else {
			// specific backport, match distribution initially
			if !strings.Contains(release, kernel.Distribution) {
				continue
			}

			// strict match version, patchlevel, sublevel, and >= match extraversion
			if version == kernel.Version && patchlevel == kernel.PatchLevel &&
				sublevel == kernel.SubLevel && extraversion >= kernel.ExtraVersion {
				return true
			}
		}
	}
	klog.Errorf("kernel %s does not support required features", release)
	return false
}

// GenerateVolID generates a volume ID based on passed in parameters and version, to be returned
// to the CO system
func GenerateVolID(ctx context.Context, monitors string, cr *Credentials, locationID int64, pool, clusterID, objUUID string, volIDVersion uint16) (string, error) {
	var err error

	if locationID == InvalidPoolID {
		locationID, err = GetPoolID(monitors, cr, pool)
		if err != nil {
			return "", err
		}
	}

	// generate the volume ID to return to the CO system
	vi := CSIIdentifier{
		LocationID:      locationID,
		EncodingVersion: volIDVersion,
		ClusterID:       clusterID,
		ObjectUUID:      objUUID,
	}

	volID, err := vi.ComposeCSIID()

	return volID, err
}

// CreateMountPoint creates the directory with given path
func CreateMountPoint(mountPath string) error {
	return os.MkdirAll(mountPath, 0750)
}

// checkDirExists checks directory  exists or not
func checkDirExists(p string) bool {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return false
	}
	return true
}

// IsMountPoint checks if the given path is mountpoint or not
func IsMountPoint(p string) (bool, error) {
	dummyMount := mount.New("")
	notMnt, err := dummyMount.IsLikelyNotMountPoint(p)
	if err != nil {
		return false, status.Error(codes.Internal, err.Error())
	}

	return !notMnt, nil
}

// Mount mounts the source to target path
func Mount(source, target, fstype string, options []string) error {
	dummyMount := mount.New("")
	return dummyMount.Mount(source, target, fstype, options)
}

// MountOptionsAdd adds the `add` mount options to the `options` and returns a
// new string. In case `add` is already present in the `options`, `add` is not
// added again.
func MountOptionsAdd(options string, add ...string) string {
	opts := strings.Split(options, ",")
	newOpts := []string{}
	// clean original options from empty strings
	for _, opt := range opts {
		if opt != "" {
			newOpts = append(newOpts, opt)
		}
	}

	for _, opt := range add {
		if opt != "" && !contains(newOpts, opt) {
			newOpts = append(newOpts, opt)
		}
	}

	return strings.Join(newOpts, ",")
}

func contains(s []string, key string) bool {
	for _, v := range s {
		if v == key {
			return true
		}
	}

	return false
}
